# OperationsApi

All URIs are relative to *http://localhost:8080/v1*

| Method | HTTP request | Description |
|------------- | ------------- | -------------|
| [**getOperation**](OperationsApi.md#getoperation) | **GET** /operations/{id} | Get an async operation status |



## getOperation

> Operation getOperation(id)

Get an async operation status

### Example

```ts
import {
  Configuration,
  OperationsApi,
} from '@gpuai/sdk';
import type { GetOperationRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const config = new Configuration({ 
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new OperationsApi(config);

  const body = {
    // string
    id: 38400000-8cf0-11bd-b23e-10b96e4ef00d,
  } satisfies GetOperationRequest;

  try {
    const data = await api.getOperation(body);
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

[**Operation**](Operation.md)

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

