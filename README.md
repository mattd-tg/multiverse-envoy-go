# Shard Router Filter

In this example, we show how the Go shard router filter can be used with the Envoy
proxy.

The example demonstrates a multi-tiered caching system that maps tenant IDs to shard IDs using:
- **Memory cache** (fastest, first-tier lookup)
- **Redis cache** (distributed, second-tier lookup)
- **S3 storage** (source of truth, third-tier lookup)

The filter supports tenant extraction from both subdomains and custom headers, and automatically injects
the `x-shard-id` header for downstream services.

## Step 1: Prepare test data and compile the plugin

First, create sample tenant-shard mapping data in S3 format:

```console
$ echo '{
  "mappings": [
    {"tenant_id": "tenant1", "shard_id": "shard-a"},
    {"tenant_id": "tenant2", "shard_id": "shard-b"},
    {"tenant_id": "tenant3", "shard_id": "shard-a"},
    {"tenant_id": "acme", "shard_id": "shard-c"}
  ]
}' > /tmp/tenant-shard-mapping.json
```

Ensure you are in the project root directory and build the shard router plugin library.

```console
$ docker compose -f docker-compose-go.yaml run --rm go_plugin_compile
```

The compiled library should now be in the `lib` folder.

```console
$ ls lib
proxy.so
```

## Step 2: Start services and upload test data

Start all the containers including Redis and Minio services.

```console
$ docker compose pull
$ docker compose up --build -d
$ docker compose ps

      Name                      Command               State                          Ports
-----------------------------------------------------------------------------------------------------------------------
golang_proxy_1         /docker-entrypoint.sh /usr ...   Up      10000/tcp, 0.0.0.0:10000->10000/tcp,:::10000->10000/tcp
golang_web_service_1   /bin/echo-server                 Up      8080/tcp
redis_1                redis-server                     Up      6379/tcp
minio_1                minio server /data               Up      9000/tcp, 9001/tcp
```

Configure Minio client and upload test data:

```console
$ mc alias set local http://localhost:9000 minioadmin minioadmin
$ mc mb local/tenant-mappings
$ mc cp /tmp/tenant-shard-mapping.json local/tenant-mappings/mappings.json
```

Verify the data was uploaded:

```console
$ mc ls local/tenant-mappings/
[2024-01-01 00:00:00 UTC]   123B mappings.json
```

You can also access the Minio web interface at http://localhost:9001 (default credentials: minioadmin/minioadmin).

## Step 3: Test tenant extraction from subdomain

Test the shard router's ability to extract tenant ID from subdomains and inject the correct shard ID.

```console
$ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "x-shard-id"
< x-shard-id: shard-a

$ curl -H "Host: acme.example.com" localhost:10000 -v 2>&1 | grep "x-shard-id"
< x-shard-id: shard-c
```

## Step 4: Test tenant extraction from headers

Test the shard router's ability to extract tenant ID from custom headers.

```console
$ curl -H "X-Tenant-ID: tenant2" localhost:10000 -v 2>&1 | grep "x-shard-id"
< x-shard-id: shard-b
```

## Step 5: Test cache tier functionality

Test the multi-tiered caching system (memory → Redis → S3).

**Memory cache test (repeat requests):**

```console
$ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "x-shard-id"
< x-shard-id: shard-a

# Second request should hit memory cache (check logs for faster response)
$ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "x-shard-id"
< x-shard-id: shard-a
```

**Redis cache verification:**

```console
$ docker exec redis_1 redis-cli get "shard_router:tenant1"
"shard-a"

$ docker exec redis_1 redis-cli get "shard_router:tenant2"
"shard-b"
```

**S3 fallback test (disable Redis temporarily):**

```console
$ docker stop redis_1
$ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "x-shard-id"
< x-shard-id: shard-a

$ docker start redis_1
```

## Step 6: Test error handling and edge cases

Test how the shard router handles various failure scenarios and edge cases.

**Unknown tenant test:**

```console
$ curl -H "Host: unknown.example.com" localhost:10000 -v 2>&1 | grep "x-shard-id"
# Should return no x-shard-id header or default behavior
```

**Invalid tenant formats:**

```console
$ curl -H "Host: .example.com" localhost:10000 -v 2>&1 | grep "x-shard-id"
# Should handle malformed subdomain gracefully

$ curl -H "X-Tenant-ID: " localhost:10000 -v 2>&1 | grep "x-shard-id"
# Should handle empty tenant ID
```

**Service dependency failures:**

```console
# Test with Minio unavailable
$ docker stop minio_1
$ curl -H "Host: newclient.example.com" localhost:10000 -v 2>&1 | grep "x-shard-id"
# Should handle S3 unavailability gracefully
$ docker start minio_1
```

**Existing shard header test:**

```console
$ curl -H "Host: tenant1.example.com" -H "x-shard-id: existing-shard" localhost:10000 -v 2>&1 | grep "x-shard-id"
< x-shard-id: existing-shard
# Should preserve existing shard header
```

