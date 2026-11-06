# RegistryCredentialsApi

All URIs are relative to *http://localhost:8080/v1*

| Method | HTTP request | Description |
|------------- | ------------- | -------------|
| [**createRegistryCredential**](RegistryCredentialsApi.md#createregistrycredentialoperation) | **POST** /registry-credentials | Store a registry credential |
| [**deleteRegistryCredential**](RegistryCredentialsApi.md#deleteregistrycredential) | **DELETE** /registry-credentials/{id} | Delete a registry credential |
| [**listRegistryCredentials**](RegistryCredentialsApi.md#listregistrycredentials) | **GET** /registry-credentials | List registry credentials |



## createRegistryCredential

> RegistryCredential createRegistryCredential(createRegistryCredentialRequest)

Store a registry credential

Stores a private-registry login for pulling private custom images. The password crosses the wire exactly once — here — and is encrypted at rest; no read ever returns it. Reference the returned id from &#x60;registry_credential_id&#x60; on instance creation. The credential\&#39;s registry host must obey the same rules as image references (no localhost/private addresses); username and password must not contain whitespace. Duplicate names return 409 &#x60;name_taken&#x60;.

### Example

```ts
import {
  Configuration,
  RegistryCredentialsApi,
} from '@gpuai/sdk';
import type { CreateRegistryCredentialOperationRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new RegistryCredentialsApi(config);

  const body = {
    // CreateRegistryCredentialRequest
    createRegistryCredentialRequest: ...,
  } satisfies CreateRegistryCredentialOperationRequest;

  try {
    const data = await api.createRegistryCredential(body);
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
| **createRegistryCredentialRequest** | [CreateRegistryCredentialRequest](CreateRegistryCredentialRequest.md) |  | |

### Return type

[**RegistryCredential**](RegistryCredential.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: `application/json`
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **201** | Credential stored (metadata only; no password) |  -  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


## deleteRegistryCredential

> deleteRegistryCredential(id)

Delete a registry credential

### Example

```ts
import {
  Configuration,
  RegistryCredentialsApi,
} from '@gpuai/sdk';
import type { DeleteRegistryCredentialRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new RegistryCredentialsApi(config);

  const body = {
    // string
    id: id_example,
  } satisfies DeleteRegistryCredentialRequest;

  try {
    const data = await api.deleteRegistryCredential(body);
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
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


## listRegistryCredentials

> ListRegistryCredentials200Response listRegistryCredentials()

List registry credentials

Lists the organization\&#39;s stored private-registry logins for custom-image launches. Passwords are never returned by any read. 503 when the feature is not enabled on this server.

### Example

```ts
import {
  Configuration,
  RegistryCredentialsApi,
} from '@gpuai/sdk';
import type { ListRegistryCredentialsRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new RegistryCredentialsApi(config);

  try {
    const data = await api.listRegistryCredentials();
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```

### Parameters

This endpoint does not need any parameter.

### Return type

[**ListRegistryCredentials200Response**](ListRegistryCredentials200Response.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

- **Content-Type**: Not defined
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **200** | Credential list |  -  |
| **429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)

