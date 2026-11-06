# InstancesApi

All URIs are relative to *http://localhost:8080/v1*

| Method | HTTP request | Description |
|------------- | ------------- | -------------|
| [**createInstance**](InstancesApi.md#createinstanceoperation) | **POST** /instances | Create an instance (async — returns 202 + Operation-Id header) |
| [**deleteInstance**](InstancesApi.md#deleteinstance) | **DELETE** /instances/{id} | Terminate an instance (idempotent) |
| [**getInstance**](InstancesApi.md#getinstance) | **GET** /instances/{id} | Get an instance |
| [**listInstances**](InstancesApi.md#listinstances) | **GET** /instances | List instances |
| [**updateInstance**](InstancesApi.md#updateinstanceoperation) | **PATCH** /instances/{id} | Update an instance (rename only in v1) |



## createInstance

> Operation createInstance(createInstanceRequest, idempotencyKey)

Create an instance (async — returns 202 + Operation-Id header)

### Example

```ts
import {
  Configuration,
  InstancesApi,
} from '@gpuai/sdk';
import type { CreateInstanceOperationRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new InstancesApi(config);

  const body = {
    // CreateInstanceRequest
    createInstanceRequest: ...,
    // string (optional)
    idempotencyKey: idempotencyKey_example,
  } satisfies CreateInstanceOperationRequest;

  try {
    const data = await api.createInstance(body);
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```

### Parameters


| Name | Type | Description  | Notes |
|------------- | ------------- | ------------- | -------------|
| **createInstanceRequest** | [CreateInstanceRequest](CreateInstanceRequest.md) |  | |
| **idempotencyKey** | `string` |  | [Optional] [Defaults to `undefined`] |

### Return type

[**Operation**](Operation.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: `application/json`
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **202** | Accepted; long-running operation |  * Operation-Id - Use GET /operations/{id} to poll <br>  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


## deleteInstance

> Operation deleteInstance(id, idempotencyKey)

Terminate an instance (idempotent)

Members can terminate only instances they created; organization admins can terminate any instance in the organization. A self-hosted deployment\&#39;s API keys belong to the default organization\&#39;s admin.

### Example

```ts
import {
  Configuration,
  InstancesApi,
} from '@gpuai/sdk';
import type { DeleteInstanceRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new InstancesApi(config);

  const body = {
    // string
    id: id_example,
    // string (optional)
    idempotencyKey: idempotencyKey_example,
  } satisfies DeleteInstanceRequest;

  try {
    const data = await api.deleteInstance(body);
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```

### Parameters


| Name | Type | Description  | Notes |
|------------- | ------------- | ------------- | -------------|
| **id** | `string` |  | [Defaults to `undefined`] |
| **idempotencyKey** | `string` |  | [Optional] [Defaults to `undefined`] |

### Return type

[**Operation**](Operation.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: Not defined
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **202** | Accepted; termination in progress |  * Operation-Id -  <br>  |
| **403** | Forbidden — members can only terminate instances they created |  -  |
| **404** | Already gone (idempotent terminate) |  -  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


## getInstance

> Instance getInstance(id, include)

Get an instance

### Example

```ts
import {
  Configuration,
  InstancesApi,
} from '@gpuai/sdk';
import type { GetInstanceRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new InstancesApi(config);

  const body = {
    // string
    id: id_example,
    // 'credentials' | Opt-in expansion. `credentials` restores connection.app_password on this single-instance read — the only surface that returns the web-console basic-auth password. Omitted by default so bare reads can be logged without leaking the secret. Any other value is a 400 validation_failed. The list endpoint and operation reads never return the password, with or without this parameter. (optional)
    include: include_example,
  } satisfies GetInstanceRequest;

  try {
    const data = await api.getInstance(body);
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```

### Parameters


| Name | Type | Description  | Notes |
|------------- | ------------- | ------------- | -------------|
| **id** | `string` |  | [Defaults to `undefined`] |
| **include** | `credentials` | Opt-in expansion. &#x60;credentials&#x60; restores connection.app_password on this single-instance read — the only surface that returns the web-console basic-auth password. Omitted by default so bare reads can be logged without leaking the secret. Any other value is a 400 validation_failed. The list endpoint and operation reads never return the password, with or without this parameter. | [Optional] [Defaults to `undefined`] [Enum: credentials] |

### Return type

[**Instance**](Instance.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: Not defined
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **200** | OK |  -  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


## listInstances

> InstancePage listInstances(cursor, limit, status)

List instances

Lists the organization\&#39;s instances. By default terminated instances are excluded; pass status&#x3D;terminated for the history or status&#x3D;all for everything.

### Example

```ts
import {
  Configuration,
  InstancesApi,
} from '@gpuai/sdk';
import type { ListInstancesRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new InstancesApi(config);

  const body = {
    // string (optional)
    cursor: cursor_example,
    // number (optional)
    limit: 56,
    // 'allocating' | 'starting' | 'running' | 'stopping' | 'stopped' | 'terminated' | 'error' | 'all' | Filter by status (default = all non-terminated). There is no \"unreachable\" filter value: an unreachable instance is a running instance whose SSH path is down, so it is listed under status=running and reads \"unreachable\" in the response body. (optional)
    status: status_example,
  } satisfies ListInstancesRequest;

  try {
    const data = await api.listInstances(body);
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```

### Parameters


| Name | Type | Description  | Notes |
|------------- | ------------- | ------------- | -------------|
| **cursor** | `string` |  | [Optional] [Defaults to `undefined`] |
| **limit** | `number` |  | [Optional] [Defaults to `50`] |
| **status** | `allocating`, `starting`, `running`, `stopping`, `stopped`, `terminated`, `error`, `all` | Filter by status (default &#x3D; all non-terminated). There is no \&quot;unreachable\&quot; filter value: an unreachable instance is a running instance whose SSH path is down, so it is listed under status&#x3D;running and reads \&quot;unreachable\&quot; in the response body. | [Optional] [Defaults to `undefined`] [Enum: allocating, starting, running, stopping, stopped, terminated, error, all] |

### Return type

[**InstancePage**](InstancePage.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: Not defined
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **200** | OK |  -  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


## updateInstance

> Instance updateInstance(id, updateInstanceRequest, idempotencyKey)

Update an instance (rename only in v1)

### Example

```ts
import {
  Configuration,
  InstancesApi,
} from '@gpuai/sdk';
import type { UpdateInstanceOperationRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new InstancesApi(config);

  const body = {
    // string
    id: id_example,
    // UpdateInstanceRequest
    updateInstanceRequest: ...,
    // string (optional)
    idempotencyKey: idempotencyKey_example,
  } satisfies UpdateInstanceOperationRequest;

  try {
    const data = await api.updateInstance(body);
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```

### Parameters


| Name | Type | Description  | Notes |
|------------- | ------------- | ------------- | -------------|
| **id** | `string` |  | [Defaults to `undefined`] |
| **updateInstanceRequest** | [UpdateInstanceRequest](UpdateInstanceRequest.md) |  | |
| **idempotencyKey** | `string` |  | [Optional] [Defaults to `undefined`] |

### Return type

[**Instance**](Instance.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: `application/json`
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **200** | OK |  -  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)

