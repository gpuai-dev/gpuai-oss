# EnvironmentsApi

All URIs are relative to *http://localhost:8080/v1*

| Method | HTTP request | Description |
|------------- | ------------- | -------------|
| [**getEnvironments**](EnvironmentsApi.md#getenvironments) | **GET** /environments | List launch environments (no auth required) |



## getEnvironments

> Environments getEnvironments()

List launch environments (no auth required)

The server-controlled launch environment catalog — the certified framework roster, the selectable version(s) per framework (the version picker), the \&quot;What\&#39;s included\&quot; software lists, and the raw-VM OS catalog. These are exactly the values accepted by the &#x60;environment&#x60; field of POST /v1/instances.

### Example

```ts
import {
  Configuration,
  EnvironmentsApi,
} from '@gpuai/sdk';
import type { GetEnvironmentsRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const api = new EnvironmentsApi();

  try {
    const data = await api.getEnvironments();
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

[**Environments**](Environments.md)

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

