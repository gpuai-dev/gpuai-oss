# gpuai_sdk.PricingApi

All URIs are relative to *http://localhost:8080/v1*

Method | HTTP request | Description
------------- | ------------- | -------------
[**list_pricing**](PricingApi.md#list_pricing) | **GET** /pricing | List pricing per GPU type + region (no auth required)


# **list_pricing**
> PricingPage list_pricing(cursor=cursor, limit=limit, gpu_type=gpu_type, region=region, tier=tier, include_unavailable=include_unavailable)

List pricing per GPU type + region (no auth required)

Lists every distinct offer (gpu_type, gpu_count, region, tier, price, boot class). Customer-identical offers are merged with availability summed. Offers with zero availability are omitted unless include_unavailable=true.

### Example


```python
import gpuai_sdk
from gpuai_sdk.models.pricing_page import PricingPage
from gpuai_sdk.rest import ApiException
from pprint import pprint

# Defining the host is optional and defaults to http://localhost:8080/v1
# See configuration.py for a list of all supported configuration parameters.
configuration = gpuai_sdk.Configuration(
    host = "http://localhost:8080/v1"
)


# Enter a context with an instance of the API client
with gpuai_sdk.ApiClient(configuration) as api_client:
    # Create an instance of the API class
    api_instance = gpuai_sdk.PricingApi(api_client)
    cursor = 'cursor_example' # str |  (optional)
    limit = 50 # int |  (optional) (default to 50)
    gpu_type = 'gpu_type_example' # str | Exact-match filter on gpu_type (optional)
    region = 'region_example' # str | Exact-match filter on canonical region code (optional)
    tier = 'tier_example' # str | Exact-match filter on tier. `spot` is soft-deprecated: suppliers may not offer spot capacity, in which case tier=spot returns no offerings; use on_demand. (optional)
    include_unavailable = False # bool | Include offers whose current availability is zero (optional) (default to False)

    try:
        # List pricing per GPU type + region (no auth required)
        api_response = api_instance.list_pricing(cursor=cursor, limit=limit, gpu_type=gpu_type, region=region, tier=tier, include_unavailable=include_unavailable)
        print("The response of PricingApi->list_pricing:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling PricingApi->list_pricing: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **cursor** | **str**|  | [optional] 
 **limit** | **int**|  | [optional] [default to 50]
 **gpu_type** | **str**| Exact-match filter on gpu_type | [optional] 
 **region** | **str**| Exact-match filter on canonical region code | [optional] 
 **tier** | **str**| Exact-match filter on tier. &#x60;spot&#x60; is soft-deprecated: suppliers may not offer spot capacity, in which case tier&#x3D;spot returns no offerings; use on_demand. | [optional] 
 **include_unavailable** | **bool**| Include offers whose current availability is zero | [optional] [default to False]

### Return type

[**PricingPage**](PricingPage.md)

### Authorization

No authorization required

### HTTP request headers

 - **Content-Type**: Not defined
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**200** | OK |  -  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

