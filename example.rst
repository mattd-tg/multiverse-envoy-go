.. _install_sandboxes_golang_http:

Shard Router Filter
===================

.. sidebar:: Requirements

   .. include:: _include/docker-env-setup-link.rst

   :ref:`curl <start_sandboxes_setup_curl>`
        Used to make HTTP requests.

   **Redis**
        Used for distributed caching of tenant-shard mappings.

   **AWS S3**
        Used as the source of truth for tenant-shard mapping data.

In this example, we show how the `Golang <https://go.dev/>`_ shard router filter can be used with the Envoy
proxy.

The example demonstrates a multi-tiered caching system that maps tenant IDs to shard IDs using:
- **Memory cache** (fastest, first-tier lookup)
- **Redis cache** (distributed, second-tier lookup)
- **S3 storage** (source of truth, third-tier lookup)

The filter supports tenant extraction from both subdomains and custom headers, and automatically injects
the ``X-SHARD-ID`` header for downstream services.

Step 1: Prepare test data and compile the plugin
************************************************

First, create sample tenant-shard mapping data in S3 format:

.. code-block:: console

   $ cat > /tmp/tenant-shard-mapping.json << 'EOF'
   {
     "mappings": [
       {"tenant_id": "tenant1", "shard_id": "shard-a"},
       {"tenant_id": "tenant2", "shard_id": "shard-b"},
       {"tenant_id": "tenant3", "shard_id": "shard-a"},
       {"tenant_id": "acme", "shard_id": "shard-c"}
     ]
   }
   EOF

Ensure you are in the project root directory and build the shard router plugin library.

.. code-block:: console

   $ docker compose -f docker-compose-go.yaml run --rm go_plugin_compile

The compiled library should now be in the ``lib`` folder.

.. code-block:: console

   $ ls lib
   simple.so

Step 2: Start services and upload test data
********************************************

Start all the containers including Redis and Minio services.

.. code-block:: console

  $ docker compose pull
  $ docker compose up --build -d
  $ docker compose ps

        Name                      Command               State                          Ports
  -----------------------------------------------------------------------------------------------------------------------
  golang_proxy_1         /docker-entrypoint.sh /usr ...   Up      10000/tcp, 0.0.0.0:10000->10000/tcp,:::10000->10000/tcp
  golang_web_service_1   /bin/echo-server                 Up      8080/tcp
  redis_1                redis-server                     Up      6379/tcp
  minio_1                minio server /data               Up      9000/tcp, 9001/tcp

Configure Minio client and upload test data:

.. code-block:: console

  $ mc alias set local http://localhost:9000 minioadmin minioadmin
  $ mc mb local/tenant-mappings
  $ mc cp /tmp/tenant-shard-mapping.json local/tenant-mappings/mappings.json

Verify the data was uploaded:

.. code-block:: console

  $ mc ls local/tenant-mappings/
  [2024-01-01 00:00:00 UTC]   123B mappings.json

You can also access the Minio web interface at http://localhost:9001 (default credentials: minioadmin/minioadmin).

Step 3: Test tenant extraction from subdomain
**********************************************

Test the shard router's ability to extract tenant ID from subdomains and inject the correct shard ID.

**Specific validation (Gemini Pro approach):**

.. code-block:: console

   $ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID: shard-a"
   < X-SHARD-ID: shard-a

   $ curl -H "Host: tenant2.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID: shard-b"
   < X-SHARD-ID: shard-b

**Broader validation (Opus approach):**

.. code-block:: console

   $ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   < X-SHARD-ID: shard-a

   $ curl -H "Host: acme.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   < X-SHARD-ID: shard-c

Step 4: Test tenant extraction from headers
*******************************************

Test the shard router's ability to extract tenant ID from custom headers.

**Specific validation:**

.. code-block:: console

   $ curl -H "X-Tenant-ID: tenant1" localhost:10000 -v 2>&1 | grep "X-SHARD-ID: shard-a"
   < X-SHARD-ID: shard-a

   $ curl -H "X-Tenant-ID: tenant3" localhost:10000 -v 2>&1 | grep "X-SHARD-ID: shard-a"
   < X-SHARD-ID: shard-a

**Broader validation:**

.. code-block:: console

   $ curl -H "X-Tenant-ID: tenant2" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   < X-SHARD-ID: shard-b

Step 5: Test cache tier functionality
*************************************

Test the multi-tiered caching system (memory → Redis → S3).

**Memory cache test (repeat requests):**

.. code-block:: console

   $ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   < X-SHARD-ID: shard-a

   # Second request should hit memory cache (check logs for faster response)
   $ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   < X-SHARD-ID: shard-a

**Redis cache verification:**

.. code-block:: console

   $ docker exec redis_1 redis-cli get "shard_router:tenant1"
   "shard-a"

   $ docker exec redis_1 redis-cli get "shard_router:tenant2"
   "shard-b"

**S3 fallback test (disable Redis temporarily):**

.. code-block:: console

   $ docker stop redis_1
   $ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   < X-SHARD-ID: shard-a

   $ docker start redis_1

Step 6: Test error handling and edge cases
******************************************

Test how the shard router handles various failure scenarios and edge cases.

**Unknown tenant test:**

.. code-block:: console

   $ curl -H "Host: unknown.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   # Should return no X-SHARD-ID header or default behavior

**Invalid tenant formats:**

.. code-block:: console

   $ curl -H "Host: .example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   # Should handle malformed subdomain gracefully

   $ curl -H "X-Tenant-ID: " localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   # Should handle empty tenant ID

**Service dependency failures:**

.. code-block:: console

   # Test with Minio unavailable
   $ docker stop minio_1
   $ curl -H "Host: newclient.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   # Should handle S3 unavailability gracefully
   $ docker start minio_1

**Existing shard header test:**

.. code-block:: console

   $ curl -H "Host: tenant1.example.com" -H "X-SHARD-ID: existing-shard" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   < X-SHARD-ID: existing-shard
   # Should preserve existing shard header

Step 7: Operational verification and monitoring
***********************************************

Verify operational aspects of the shard router.

**Cache TTL expiration test:**

.. code-block:: console

   # Wait for Redis TTL to expire (default 5 minutes) or manually expire
   $ docker exec redis_1 redis-cli del "shard_router:tenant1"
   $ curl -H "Host: tenant1.example.com" localhost:10000 -v 2>&1 | grep "X-SHARD-ID"
   < X-SHARD-ID: shard-a

   # Verify cache was repopulated
   $ docker exec redis_1 redis-cli get "shard_router:tenant1"
   "shard-a"

**Logging verification:**

.. code-block:: console

   $ docker logs golang_proxy_1 2>&1 | grep "shard_router"
   # Should show cache hits, misses, and tenant extraction logs

**Performance characteristics test:**

.. code-block:: console

   # Test multiple requests to observe cache performance
   $ time curl -H "Host: tenant1.example.com" localhost:10000 -s > /dev/null
   $ time curl -H "Host: tenant1.example.com" localhost:10000 -s > /dev/null
   # Second request should be faster due to memory cache

**Cache warming procedure:**

.. code-block:: console

   # Warm cache for all known tenants
   $ for tenant in tenant1 tenant2 tenant3 acme; do
       curl -H "Host: $tenant.example.com" localhost:10000 -s > /dev/null
       echo "Warmed cache for $tenant"
     done

   # Verify all tenants are cached
   $ docker exec redis_1 redis-cli keys "shard_router:*"
   1) "shard_router:tenant1"
   2) "shard_router:tenant2"
   3) "shard_router:tenant3"
   4) "shard_router:acme"

Troubleshooting
***************

If you encounter issues:

1. **Check service health:**

   .. code-block:: console

      $ docker ps
      $ docker logs golang_proxy_1
      $ docker logs redis_1
      $ docker logs minio_1

2. **Verify data in Minio:**

   .. code-block:: console

      $ mc ls local/tenant-mappings/
      $ mc cat local/tenant-mappings/mappings.json

3. **Check Redis cache:**

   .. code-block:: console

      $ docker exec redis_1 redis-cli keys "*"
      $ docker exec redis_1 redis-cli flushall  # Clear cache if needed

4. **Test connectivity:**

   .. code-block:: console

      $ curl -v localhost:10000/health  # If health endpoint exists
      $ docker exec golang_proxy_1 curl -v localhost:9000  # Test Minio from proxy

.. seealso::

   :ref:`Envoy Go filter <config_http_filters_golang>`
      Further information about the Envoy Go filter.
   :ref:`Go extension API <envoy_v3_api_file_contrib/envoy/extensions/filters/http/golang/v3alpha/golang.proto>`
      The Go extension filter API.
   :repo:`Go plugin API <contrib/golang/common/go/api/filter.go>`
      Overview of Envoy's Go plugin APIs.
