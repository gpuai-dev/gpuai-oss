# GpuTypesApi

All URIs are relative to *http://localhost:8080/v1*

| Method | HTTP request | Description |
|------------- | ------------- | -------------|
| [**listGpuTypes**](GpuTypesApi.md#listgputypes) | **GET** /gpu-types | List available GPU types (no auth required) |



## listGpuTypes

> GPUTypePage listGpuTypes(cursor, limit)

List available GPU types (no auth required)

### Example

```ts
import {
  Configuration,
  GpuTypesApi,
} from '@gpuai/sdk';
import type { ListGpuTypesRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const api = new GpuTypesApi();

  const body = {
    // string (optional)
    cursor: cursor_example,
    // number (optional)
    limit: 56,
  } satisfies ListGpuTypesRequest;

  try {
    const data = await api.listGpuTypes(body);
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

[**GPUTypePage**](GPUTypePage.md)

### Authorization

No authorization required

### HTTP request headers

- **Content-Type**: Not defined
- **Accept**: `application/json`, `application/problem+json`


### HTTP response details
| Status code | Description | Response headers |
|-------------|-------------|------------------|
| **200** | OK |  -  |
| **0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)

