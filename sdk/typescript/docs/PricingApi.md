# PricingApi

All URIs are relative to *http://localhost:8080/v1*

| Method | HTTP request | Description |
|------------- | ------------- | -------------|
| [**listPricing**](PricingApi.md#listpricing) | **GET** /pricing | List pricing per GPU type + region (no auth required) |



## listPricing

> PricingPage listPricing(cursor, limit, gpuType, region, tier, includeUnavailable)

List pricing per GPU type + region (no auth required)

Lists every distinct offer (gpu_type, gpu_count, region, tier, price, boot class). Customer-identical offers are merged with availability summed. Offers with zero availability are omitted unless include_unavailable&#x3D;true.

### Example

```ts
import {
  Configuration,
  PricingApi,
} from '@gpuai/sdk';
import type { ListPricingRequest } from '@gpuai/sdk';

async function example() {
  console.log("🚀 Testing @gpuai/sdk SDK...");
  const api = new PricingApi();

  const body = {
    // string (optional)
    cursor: cursor_example,
    // number (optional)
    limit: 56,
    // string | Exact-match filter on gpu_type (optional)
    gpuType: gpuType_example,
    // string | Exact-match filter on canonical region code (optional)
    region: region_example,
    // 'spot' | 'on_demand' | Exact-match filter on tier. `spot` is soft-deprecated: suppliers may not offer spot capacity, in which case tier=spot returns no offerings; use on_demand. (optional)
    tier: tier_example,
    // boolean | Include offers whose current availability is zero (optional)
    includeUnavailable: true,
  } satisfies ListPricingRequest;

  try {
    const data = await api.listPricing(body);
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
| **gpuType** | `string` | Exact-match filter on gpu_type | [Optional] [Defaults to `undefined`] |
| **region** | `string` | Exact-match filter on canonical region code | [Optional] [Defaults to `undefined`] |
| **tier** | `spot`, `on_demand` | Exact-match filter on tier. &#x60;spot&#x60; is soft-deprecated: suppliers may not offer spot capacity, in which case tier&#x3D;spot returns no offerings; use on_demand. | [Optional] [Defaults to `undefined`] [Enum: spot, on_demand] |
| **includeUnavailable** | `boolean` | Include offers whose current availability is zero | [Optional] [Defaults to `false`] |

### Return type

[**PricingPage**](PricingPage.md)

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

