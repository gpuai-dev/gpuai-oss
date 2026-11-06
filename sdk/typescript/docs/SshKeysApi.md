# SshKeysApi

All URIs are relative to *http://localhost:8080/v1*

| Method | HTTP request | Description |
|------------- | ------------- | -------------|
| [**createSshKey**](SshKeysApi.md#createsshkeyoperation) | **POST** /ssh-keys | Create an SSH key |
| [**deleteSshKey**](SshKeysApi.md#deletesshkey) | **DELETE** /ssh-keys/{id} | Delete an SSH key (idempotent; 404 when already gone) |
| [**getSshKey**](SshKeysApi.md#getsshkey) | **GET** /ssh-keys/{id} | Get an SSH key |
| [**listSshKeys**](SshKeysApi.md#listsshkeys) | **GET** /ssh-keys | List SSH keys |



## createSshKey

> SSHKey createSshKey(createSshKeyRequest, idempotencyKey)

Create an SSH key

### Example

```ts
import {
  Configuration,
  SshKeysApi,
} from '@gpuai/sdk';
import type { CreateSshKeyOperationRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new SshKeysApi(config);

  const body = {
    // CreateSshKeyRequest
    createSshKeyRequest: ...,
    // string (optional)
    idempotencyKey: idempotencyKey_example,
  } satisfies CreateSshKeyOperationRequest;

  try {
    const data = await api.createSshKey(body);
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
| **createSshKeyRequest** | [CreateSshKeyRequest](CreateSshKeyRequest.md) |  | |
| **idempotencyKey** | `string` |  | [Optional] [Defaults to `undefined`] |

### Return type

[**SSHKey**](SSHKey.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: `application/json`
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **201** | Created |  -  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


## deleteSshKey

> deleteSshKey(id)

Delete an SSH key (idempotent; 404 when already gone)

### Example

```ts
import {
  Configuration,
  SshKeysApi,
} from '@gpuai/sdk';
import type { DeleteSshKeyRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new SshKeysApi(config);

  const body = {
    // string
    id: id_example,
  } satisfies DeleteSshKeyRequest;

  try {
    const data = await api.deleteSshKey(body);
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

### Return type

`void` (Empty response body)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: Not defined
- **Accept**: `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **204** | Deleted |  -  |
| **404** | Already deleted (idempotent) |  -  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


## getSshKey

> SSHKey getSshKey(id)

Get an SSH key

### Example

```ts
import {
  Configuration,
  SshKeysApi,
} from '@gpuai/sdk';
import type { GetSshKeyRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new SshKeysApi(config);

  const body = {
    // string
    id: id_example,
  } satisfies GetSshKeyRequest;

  try {
    const data = await api.getSshKey(body);
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

### Return type

[**SSHKey**](SSHKey.md)

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


## listSshKeys

> SSHKeyPage listSshKeys(cursor, limit)

List SSH keys

Returns the SSH keys owned by the user the API key belongs to.

### Example

```ts
import {
  Configuration,
  SshKeysApi,
} from '@gpuai/sdk';
import type { ListSshKeysRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new SshKeysApi(config);

  const body = {
    // string (optional)
    cursor: cursor_example,
    // number (optional)
    limit: 56,
  } satisfies ListSshKeysRequest;

  try {
    const data = await api.listSshKeys(body);
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

### Return type

[**SSHKeyPage**](SSHKeyPage.md)

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

