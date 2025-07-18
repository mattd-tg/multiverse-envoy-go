package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"github.com/redis/go-redis/v9"
)

// extractTenantFromHost extracts tenant ID from the Host header
func (f *ShardRouterFilter) extractTenantFromHost(host string) (string, error) {
	if f.config.TenantExtractionMode == "subdomain" {
		// Extract tenant from subdomain (e.g., tenant.example.com -> tenant)
		parts := strings.Split(host, ".")
		if len(parts) >= 2 {
			// Remove port if present
			tenant := strings.Split(parts[0], ":")[0]
			if tenant != "" {
				return tenant, nil
			}
		}
		return "", fmt.Errorf("unable to extract tenant from host: %s", host)
	}
	return "", fmt.Errorf("unsupported tenant extraction mode: %s", f.config.TenantExtractionMode)
}

// lookupInMemoryCache checks the in-memory cache for tenant-shard mapping
func (f *ShardRouterFilter) lookupInMemoryCache(tenantID string) (string, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	if f.memoryCache != nil {
		shardID, found := f.memoryCache.Get(tenantID)
		if found {
			api.LogDebugf("Memory cache hit for tenant: %s -> shard: %s", tenantID, shardID)
			return shardID, true
		}
	}
	api.LogDebugf("Memory cache miss for tenant: %s", tenantID)
	return "", false
}

// lookupInRedisCache checks the Redis cache for tenant-shard mapping
func (f *ShardRouterFilter) lookupInRedisCache(tenantID string) (string, error) {
	if f.redisClient == nil {
		return "", fmt.Errorf("redis client not initialized")
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), f.config.RedisTimeout)
	defer cancel()
	
	key := f.config.RedisKeyPrefix + tenantID
	result := f.redisClient.Get(ctx, key)
	
	if result.Err() == redis.Nil {
		api.LogDebugf("Redis cache miss for tenant: %s", tenantID)
		return "", nil
	} else if result.Err() != nil {
		api.LogWarnf("Redis lookup error for tenant %s: %v", tenantID, result.Err())
		return "", result.Err()
	}
	
	shardID, err := result.Result()
	if err != nil {
		return "", err
	}
	
	api.LogDebugf("Redis cache hit for tenant: %s -> shard: %s", tenantID, shardID)
	return shardID, nil
}

// cacheInRedis stores tenant-shard mapping in Redis
func (f *ShardRouterFilter) cacheInRedis(tenantID, shardID string) error {
	if f.redisClient == nil {
		return fmt.Errorf("redis client not initialized")
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), f.config.RedisTimeout)
	defer cancel()
	
	key := f.config.RedisKeyPrefix + tenantID
	err := f.redisClient.Set(ctx, key, shardID, f.config.RedisTTL).Err()
	if err != nil {
		api.LogWarnf("Failed to cache in Redis for tenant %s: %v", tenantID, err)
		return err
	}
	
	api.LogDebugf("Cached in Redis: tenant %s -> shard %s", tenantID, shardID)
	return nil
}

// lookupInS3 fetches the complete mapping from S3 and searches for the tenant
func (f *ShardRouterFilter) lookupInS3(tenantID string) (string, error) {
	if f.s3Client == nil {
		return "", fmt.Errorf("s3 client not initialized")
	}
	
	ctx, cancel := context.WithTimeout(context.Background(), f.config.S3Timeout)
	defer cancel()
	
	input := &s3.GetObjectInput{
		Bucket: aws.String(f.config.S3Bucket),
		Key:    aws.String(f.config.S3Key),
	}
	
	result, err := f.s3Client.GetObjectWithContext(ctx, input)
	if err != nil {
		api.LogWarnf("Failed to fetch mapping from S3: %v", err)
		return "", err
	}
	defer result.Body.Close()
	
	body, err := io.ReadAll(result.Body)
	if err != nil {
		api.LogWarnf("Failed to read S3 object body: %v", err)
		return "", err
	}
	
	var mappingData MappingData
	if err := json.Unmarshal(body, &mappingData); err != nil {
		api.LogWarnf("Failed to parse mapping data from S3: %v", err)
		return "", err
	}
	
	// Search for the tenant in the mappings
	for _, mapping := range mappingData.Mappings {
		if mapping.TenantID == tenantID {
			api.LogDebugf("S3 lookup hit for tenant: %s -> shard: %s", tenantID, mapping.ShardID)
			return mapping.ShardID, nil
		}
	}
	
	api.LogDebugf("S3 lookup miss for tenant: %s", tenantID)
	return "", nil
}

// cacheInMemory stores tenant-shard mapping in memory cache
func (f *ShardRouterFilter) cacheInMemory(tenantID, shardID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	
	if f.memoryCache != nil {
		f.memoryCache.Add(tenantID, shardID)
		api.LogDebugf("Cached in memory: tenant %s -> shard %s", tenantID, shardID)
	}
}

// orchestratedLookup performs the complete lookup strategy with fallback
func (f *ShardRouterFilter) orchestratedLookup(tenantID string) (string, error) {
	// Tier 1: Memory cache lookup
	if shardID, found := f.lookupInMemoryCache(tenantID); found {
		return shardID, nil
	}
	
	// Tier 2: Redis cache lookup
	shardID, err := f.lookupInRedisCache(tenantID)
	if err != nil {
		api.LogWarnf("Redis lookup failed for tenant %s: %v", tenantID, err)
	} else if shardID != "" {
		// Cache in memory for faster future lookups
		f.cacheInMemory(tenantID, shardID)
		return shardID, nil
	}
	
	// Tier 3: S3 lookup (source of truth)
	shardID, err = f.lookupInS3(tenantID)
	if err != nil {
		api.LogWarnf("S3 lookup failed for tenant %s: %v", tenantID, err)
		return "", err
	}
	
	if shardID != "" {
		// Cache in both Redis and memory
		if err := f.cacheInRedis(tenantID, shardID); err != nil {
			api.LogWarnf("Failed to cache in Redis: %v", err)
		}
		f.cacheInMemory(tenantID, shardID)
		return shardID, nil
	}
	
	// No mapping found
	return "", fmt.Errorf("no shard mapping found for tenant: %s", tenantID)
}

// DecodeHeaders is the main entry point for processing requests
func (f *ShardRouterFilter) DecodeHeaders(header api.RequestHeaderMap, endStream bool) api.StatusType {
	// Check if X-SHARD-ID header is already present
	if existingShardID, exists := header.Get("X-SHARD-ID"); exists {
		api.LogDebugf("X-SHARD-ID header already present: %s", existingShardID)
		return api.Continue
	}
	
	// Extract tenant ID from request
	var tenantID string
	var err error
	
	if f.config.TenantExtractionMode == "header" {
		// Extract tenant from header
		if headerTenantID, exists := header.Get(f.config.TenantHeaderName); exists {
			tenantID = headerTenantID
		} else {
			api.LogWarnf("Tenant header %s not found", f.config.TenantHeaderName)
			return api.Continue
		}
	} else {
		// Extract tenant from Host header
		if host, exists := header.Get(":authority"); exists {
			tenantID, err = f.extractTenantFromHost(host)
			if err != nil {
				api.LogWarnf("Failed to extract tenant from host %s: %v", host, err)
				return api.Continue
			}
		} else {
			api.LogWarnf("Host header not found")
			return api.Continue
		}
	}
	
	if tenantID == "" {
		api.LogWarnf("Unable to determine tenant ID")
		return api.Continue
	}
	
	api.LogDebugf("Extracted tenant ID: %s", tenantID)
	
	// Perform orchestrated lookup for shard ID
	shardID, err := f.orchestratedLookup(tenantID)
	if err != nil {
		api.LogWarnf("Failed to lookup shard for tenant %s: %v", tenantID, err)
		return api.Continue
	}
	
	// Store shard ID for response headers
	f.currentShardID = shardID
	api.LogDebugf("Found shard ID: %s for tenant: %s", shardID, tenantID)
	
	return api.Continue
}

// DecodeData handles request body processing
func (f *ShardRouterFilter) DecodeData(buffer api.BufferInstance, endStream bool) api.StatusType {
	return api.Continue
}

// DecodeTrailers handles request trailers
func (f *ShardRouterFilter) DecodeTrailers(trailers api.RequestTrailerMap) api.StatusType {
	return api.Continue
}

// EncodeHeaders handles response headers
func (f *ShardRouterFilter) EncodeHeaders(header api.ResponseHeaderMap, endStream bool) api.StatusType {
	// Add X-SHARD-ID header if we found a shard for this request
	if f.currentShardID != "" {
		header.Set("X-SHARD-ID", f.currentShardID)
		api.LogDebugf("Added X-SHARD-ID response header: %s", f.currentShardID)
	}
	return api.Continue
}

// EncodeData handles response body processing
func (f *ShardRouterFilter) EncodeData(buffer api.BufferInstance, endStream bool) api.StatusType {
	return api.Continue
}

// EncodeTrailers handles response trailers
func (f *ShardRouterFilter) EncodeTrailers(trailers api.ResponseTrailerMap) api.StatusType {
	return api.Continue
}

// OnLog is called when the HTTP stream is ended
func (f *ShardRouterFilter) OnLog(reqHeader api.RequestHeaderMap, reqTrailer api.RequestTrailerMap, respHeader api.ResponseHeaderMap, respTrailer api.ResponseTrailerMap) {
	// Log metrics and monitoring information
	if shardID, exists := reqHeader.Get("X-SHARD-ID"); exists {
		api.LogDebugf("Request processed with shard ID: %s", shardID)
	}
}

// OnLogDownstreamStart is called when a new HTTP request is received
func (f *ShardRouterFilter) OnLogDownstreamStart(reqHeader api.RequestHeaderMap) {
	// Log request start
}

// OnLogDownstreamPeriodic is called periodically during request processing
func (f *ShardRouterFilter) OnLogDownstreamPeriodic(reqHeader api.RequestHeaderMap, reqTrailer api.RequestTrailerMap, respHeader api.ResponseHeaderMap, respTrailer api.ResponseTrailerMap) {
	// Log periodic information
}

// OnDestroy is called when the filter is being destroyed
func (f *ShardRouterFilter) OnDestroy(reason api.DestroyReason) {
	// Cleanup resources
	if f.redisClient != nil {
		f.redisClient.Close()
	}
	
	api.LogDebugf("ShardRouterFilter destroyed, reason: %v", reason)
}

