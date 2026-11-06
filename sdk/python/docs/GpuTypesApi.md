# gpuai_sdk.GpuTypesApi

All URIs are relative to *http://localhost:8080/v1*

Method | HTTP request | Description
------------- | ------------- | -------------
[**list_gpu_types**](GpuTypesApi.md#list_gpu_types) | **GET** /gpu-types | List available GPU types (no auth required)


# **list_gpu_types**
> GPUTypePage list_gpu_types(cursor=cursor, limit=limit)

List available GPU types (no auth required)

### Example


```python
import gpuai_sdk
from gpuai_sdk.models.gpu_type_page import GPUTypePage
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
    api_instance = gpuai_sdk.GpuTypesApi(api_client)
    cursor = 'cursor_example' # str |  (optional)
    limit = 50 # int |  (optional) (default to 50)

    try:
        # List available GPU types (no auth required)
        api_response = api_instance.list_gpu_types(cursor=cursor, limit=limit)
        print("The response of GpuTypesApi->list_gpu_types:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling GpuTypesApi->list_gpu_types: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **cursor** | **str**|  | [optional] 
 **limit** | **int**|  | [optional] [default to 50]

### Return type

[**GPUTypePage**](GPUTypePage.md)

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

