# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

This is an Envoy proxy Go plugin project that compiles to a shared library (.so file).

### Core Build Process
```bash
# Compile the Go plugin to shared library
docker-compose -f docker-compose-go.yaml up go_plugin_compile

# Or manually in the proxy/ directory:
cd proxy
go build -o ../lib/proxy.so -buildmode=c-shared .
```

### Testing and Verification
```bash
# Start all containers for testing
docker-compose up --build -d

# Test tenant extraction from subdomain
curl -H "Host: tenant1.example.com" http://localhost:10000/ -v 2>&1 | grep "x-shard-id"

# Test tenant extraction from headers
curl -H "X-Tenant-ID: tenant2" http://localhost:10000/ -v 2>&1 | grep "x-shard-id"

# Verify Redis cache contents
docker exec redis_1 redis-cli get "shard_router:tenant1"
```

## Architecture Overview

This is a multi-tiered tenant-to-shard routing system implemented as an Envoy Go plugin:

### Core Components

**Filter Implementation** (`proxy/filter.go`):
- `ShardRouterFilter`: Main filter implementing multi-tiered caching (memory → Redis → S3)
- Implements Envoy's `StreamFilter` interface with decode/encode methods
- Extracts tenant IDs from subdomains or headers
- Adds `x-shard-id` headers to route requests to appropriate shards

**Configuration** (`proxy/config.go`):
- `PluginConfig`: Configuration parser for S3, Redis, and caching settings
- Supports both subdomain and header-based tenant extraction
- Configurable timeouts, TTLs, and cache sizes

### Data Flow

1. **Request Processing** (`DecodeHeaders`):
   - Extract tenant ID from Host header subdomain or custom header
   - Perform tiered lookup: Memory cache → Redis cache → S3 (source of truth)
   - Cache results in faster tiers for subsequent requests

2. **Response Processing** (`EncodeHeaders`):
   - Add `x-shard-id` header to responses indicating which shard handled the request

### Storage Layers

- **Memory Cache**: LRU cache for fastest access (configurable size)
- **Redis**: Distributed cache with TTL for cross-instance sharing
- **S3**: Source of truth containing JSON mapping file with tenant-to-shard mappings

### Configuration Structure

The plugin expects JSON configuration with these key sections:
- `s3_bucket`, `s3_key`, `s3_region`: S3 source of truth location
- `redis_addr`, `redis_password`, `redis_db`: Redis caching configuration  
- `memory_cache_size`, `redis_ttl`: Cache behavior tuning
- `tenant_extraction_mode`: "subdomain" or "header" for tenant ID extraction
- `redis_timeout`, `s3_timeout`: Operation timeouts

### Development Environment

- Uses Docker Compose for containerized development
- Go 1.22+ required (specified in go.mod)
- Builds targeting Envoy's Go plugin interface
- Example service setup includes TypeScript/Bun services for testing
- Redis (port 6379) and Minio/S3 (ports 9000/9001) services for caching and storage
- Main proxy runs on port 10000

### Test Data Setup

For testing, create sample tenant-shard mapping in Minio:
```bash
# Create test data file
cat > /tmp/tenant-shard-mapping.json << 'EOF'
{
  "mappings": [
    {"tenant_id": "tenant1", "shard_id": "shard-a"},
    {"tenant_id": "tenant2", "shard_id": "shard-b"},
    {"tenant_id": "acme", "shard_id": "shard-c"}
  ]
}
EOF

# Upload to Minio (after docker-compose up)
mc alias set local http://localhost:9000 minioadmin minioadmin
mc mb local/tenant-mappings
mc cp /tmp/tenant-shard-mapping.json local/tenant-mappings/mappings.json
```

The plugin is designed for high-performance tenant routing in multi-tenant architectures where requests need to be directed to specific shards based on tenant identification.
