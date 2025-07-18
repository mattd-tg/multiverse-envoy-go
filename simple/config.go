package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	xds "github.com/cncf/xds/go/xds/type/v3"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"github.com/envoyproxy/envoy/contrib/golang/filters/http/source/go/pkg/http"
	"github.com/hashicorp/golang-lru/v2"
	"github.com/redis/go-redis/v9"
)

const Name = "shard_router"

func init() {
	http.RegisterHttpFilterFactoryAndConfigParser(Name, filterFactory, &parser{})
}

// TenantShardMapping represents individual tenant to shard mapping
type TenantShardMapping struct {
	TenantID string `json:"tenant_id"`
	ShardID  string `json:"shard_id"`
}

// MappingData represents the complete mapping data structure from S3
type MappingData struct {
	Mappings []TenantShardMapping `json:"mappings"`
}

// PluginConfig represents the plugin configuration
type PluginConfig struct {
	// S3 configuration
	S3Bucket   string `json:"s3_bucket"`
	S3Key      string `json:"s3_key"`
	S3Region   string `json:"s3_region"`
	S3Endpoint string `json:"s3_endpoint"`
	
	// Redis configuration
	RedisAddr     string `json:"redis_addr"`
	RedisPassword string `json:"redis_password"`
	RedisDB       int    `json:"redis_db"`
	RedisKeyPrefix string `json:"redis_key_prefix"`
	
	// Cache configuration
	MemoryCacheSize int           `json:"memory_cache_size"`
	RedisTTL        time.Duration `json:"redis_ttl"`
	
	// Tenant extraction configuration
	TenantExtractionMode string `json:"tenant_extraction_mode"` // "subdomain" or "header"
	TenantHeaderName     string `json:"tenant_header_name"`
	
	// Timeouts
	RedisTimeout time.Duration `json:"redis_timeout"`
	S3Timeout    time.Duration `json:"s3_timeout"`
}

// ShardRouterFilter represents the main filter with multi-tiered caching
type ShardRouterFilter struct {
	api.PassThroughStreamFilter
	
	callbacks api.FilterCallbackHandler
	config    *PluginConfig
	
	// Caching layers
	memoryCache *lru.Cache[string, string]
	redisClient *redis.Client
	s3Client    *s3.S3
	
	// Current request state
	currentShardID string
	
	// Synchronization
	mu sync.RWMutex
}

type parser struct {
}

// Parse the filter configuration
func (p *parser) Parse(any *anypb.Any, callbacks api.ConfigCallbackHandler) (interface{}, error) {
	configStruct := &xds.TypedStruct{}
	if err := any.UnmarshalTo(configStruct); err != nil {
		return nil, err
	}

	v := configStruct.Value
	conf := &PluginConfig{}
	
	// Parse S3 configuration
	if s3Bucket, ok := v.AsMap()["s3_bucket"]; ok {
		if str, ok := s3Bucket.(string); ok {
			conf.S3Bucket = str
		} else {
			return nil, errors.New("s3_bucket must be a string")
		}
	} else {
		return nil, errors.New("missing s3_bucket")
	}
	
	if s3Key, ok := v.AsMap()["s3_key"]; ok {
		if str, ok := s3Key.(string); ok {
			conf.S3Key = str
		} else {
			return nil, errors.New("s3_key must be a string")
		}
	} else {
		return nil, errors.New("missing s3_key")
	}
	
	if s3Region, ok := v.AsMap()["s3_region"]; ok {
		if str, ok := s3Region.(string); ok {
			conf.S3Region = str
		} else {
			return nil, errors.New("s3_region must be a string")
		}
	} else {
		conf.S3Region = "us-east-1" // default
	}
	
	if s3Endpoint, ok := v.AsMap()["s3_endpoint"]; ok {
		if str, ok := s3Endpoint.(string); ok {
			conf.S3Endpoint = str
		} else {
			return nil, errors.New("s3_endpoint must be a string")
		}
	}
	
	// Parse Redis configuration
	if redisAddr, ok := v.AsMap()["redis_addr"]; ok {
		if str, ok := redisAddr.(string); ok {
			conf.RedisAddr = str
		} else {
			return nil, errors.New("redis_addr must be a string")
		}
	} else {
		return nil, errors.New("missing redis_addr")
	}
	
	if redisPassword, ok := v.AsMap()["redis_password"]; ok {
		if str, ok := redisPassword.(string); ok {
			conf.RedisPassword = str
		}
	}
	
	if redisDB, ok := v.AsMap()["redis_db"]; ok {
		if num, ok := redisDB.(float64); ok {
			conf.RedisDB = int(num)
		} else {
			return nil, errors.New("redis_db must be a number")
		}
	}
	
	if redisKeyPrefix, ok := v.AsMap()["redis_key_prefix"]; ok {
		if str, ok := redisKeyPrefix.(string); ok {
			conf.RedisKeyPrefix = str
		}
	} else {
		conf.RedisKeyPrefix = "shard_router:"
	}
	
	// Parse cache configuration
	if cacheSize, ok := v.AsMap()["memory_cache_size"]; ok {
		if num, ok := cacheSize.(float64); ok {
			conf.MemoryCacheSize = int(num)
		} else {
			return nil, errors.New("memory_cache_size must be a number")
		}
	} else {
		conf.MemoryCacheSize = 1000 // default
	}
	
	if redisTTL, ok := v.AsMap()["redis_ttl"]; ok {
		if str, ok := redisTTL.(string); ok {
			ttl, err := time.ParseDuration(str)
			if err != nil {
				return nil, fmt.Errorf("invalid redis_ttl format: %v", err)
			}
			conf.RedisTTL = ttl
		} else {
			return nil, errors.New("redis_ttl must be a string duration")
		}
	} else {
		conf.RedisTTL = 5 * time.Minute // default
	}
	
	// Parse tenant extraction configuration
	if mode, ok := v.AsMap()["tenant_extraction_mode"]; ok {
		if str, ok := mode.(string); ok {
			conf.TenantExtractionMode = str
		} else {
			return nil, errors.New("tenant_extraction_mode must be a string")
		}
	} else {
		conf.TenantExtractionMode = "subdomain" // default
	}
	
	if headerName, ok := v.AsMap()["tenant_header_name"]; ok {
		if str, ok := headerName.(string); ok {
			conf.TenantHeaderName = str
		}
	} else {
		conf.TenantHeaderName = "X-Tenant-ID"
	}
	
	// Parse timeouts
	if redisTimeout, ok := v.AsMap()["redis_timeout"]; ok {
		if str, ok := redisTimeout.(string); ok {
			timeout, err := time.ParseDuration(str)
			if err != nil {
				return nil, fmt.Errorf("invalid redis_timeout format: %v", err)
			}
			conf.RedisTimeout = timeout
		} else {
			return nil, errors.New("redis_timeout must be a string duration")
		}
	} else {
		conf.RedisTimeout = 2 * time.Second // default
	}
	
	if s3Timeout, ok := v.AsMap()["s3_timeout"]; ok {
		if str, ok := s3Timeout.(string); ok {
			timeout, err := time.ParseDuration(str)
			if err != nil {
				return nil, fmt.Errorf("invalid s3_timeout format: %v", err)
			}
			conf.S3Timeout = timeout
		} else {
			return nil, errors.New("s3_timeout must be a string duration")
		}
	} else {
		conf.S3Timeout = 5 * time.Second // default
	}
	
	return conf, nil
}

// Merge configuration from the inherited parent configuration
func (p *parser) Merge(parent interface{}, child interface{}) interface{} {
	parentConfig := parent.(*PluginConfig)
	childConfig := child.(*PluginConfig)

	// copy one, do not update parentConfig directly.
	newConfig := *parentConfig
	
	// Override with child configuration values
	if childConfig.S3Bucket != "" {
		newConfig.S3Bucket = childConfig.S3Bucket
	}
	if childConfig.S3Key != "" {
		newConfig.S3Key = childConfig.S3Key
	}
	if childConfig.S3Region != "" {
		newConfig.S3Region = childConfig.S3Region
	}
	if childConfig.S3Endpoint != "" {
		newConfig.S3Endpoint = childConfig.S3Endpoint
	}
	if childConfig.RedisAddr != "" {
		newConfig.RedisAddr = childConfig.RedisAddr
	}
	if childConfig.RedisPassword != "" {
		newConfig.RedisPassword = childConfig.RedisPassword
	}
	if childConfig.RedisDB != 0 {
		newConfig.RedisDB = childConfig.RedisDB
	}
	if childConfig.RedisKeyPrefix != "" {
		newConfig.RedisKeyPrefix = childConfig.RedisKeyPrefix
	}
	if childConfig.MemoryCacheSize != 0 {
		newConfig.MemoryCacheSize = childConfig.MemoryCacheSize
	}
	if childConfig.RedisTTL != 0 {
		newConfig.RedisTTL = childConfig.RedisTTL
	}
	if childConfig.TenantExtractionMode != "" {
		newConfig.TenantExtractionMode = childConfig.TenantExtractionMode
	}
	if childConfig.TenantHeaderName != "" {
		newConfig.TenantHeaderName = childConfig.TenantHeaderName
	}
	if childConfig.RedisTimeout != 0 {
		newConfig.RedisTimeout = childConfig.RedisTimeout
	}
	if childConfig.S3Timeout != 0 {
		newConfig.S3Timeout = childConfig.S3Timeout
	}
	
	return &newConfig
}

func filterFactory(c interface{}, callbacks api.FilterCallbackHandler) api.StreamFilter {
	conf, ok := c.(*PluginConfig)
	if !ok {
		panic("unexpected config type")
	}
	
	// Initialize memory cache
	memoryCache, err := lru.New[string, string](conf.MemoryCacheSize)
	if err != nil {
		panic(fmt.Sprintf("failed to create memory cache: %v", err))
	}
	
	// Initialize Redis client
	redisClient := redis.NewClient(&redis.Options{
		Addr:     conf.RedisAddr,
		Password: conf.RedisPassword,
		DB:       conf.RedisDB,
	})
	
	// Initialize S3 client
	awsConfig := &aws.Config{
		Region: aws.String(conf.S3Region),
	}
	
	// Configure custom endpoint for Minio compatibility
	if conf.S3Endpoint != "" {
		awsConfig.Endpoint = aws.String(conf.S3Endpoint)
		awsConfig.S3ForcePathStyle = aws.Bool(true)
	}
	
	sess, err := session.NewSession(awsConfig)
	if err != nil {
		panic(fmt.Sprintf("failed to create AWS session: %v", err))
	}
	s3Client := s3.New(sess)
	
	return &ShardRouterFilter{
		callbacks:   callbacks,
		config:      conf,
		memoryCache: memoryCache,
		redisClient: redisClient,
		s3Client:    s3Client,
	}
}

func main() {}

