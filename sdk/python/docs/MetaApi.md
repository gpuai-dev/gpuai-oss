# gpuai_sdk.MetaApi

All URIs are relative to *http://localhost:8080/v1*

Method | HTTP request | Description
------------- | ------------- | -------------
[**get_health**](MetaApi.md#get_health) | **GET** /health | Liveness probe
[**get_open_api_spec**](MetaApi.md#get_open_api_spec) | **GET** /openapi.json | OpenAPI 3.1 specification


# **get_health**
> GetHealth200Response get_health()

Liveness probe

### Example


```python
import gpuai_sdk
from gpuai_sdk.models.get_health200_response import GetHealth200Response
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
    api_instance = gpuai_sdk.MetaApi(api_client)

    try:
        # Liveness probe
        api_response = api_instance.get_health()
        print("The response of MetaApi->get_health:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling MetaApi->get_health: %s\n" % e)
```



### Parameters

This endpoint does not need any parameter.

### Return type

[**GetHealth200Response**](GetHealth200Response.md)

### Authorization

No authorization required

### HTTP request headers

 - **Content-Type**: Not defined
 - **Accept**: application/json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**200** | OK |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **get_open_api_spec**
> object get_open_api_spec()

OpenAPI 3.1 specification

### Example


```python
import gpuai_sdk
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
    api_instance = gpuai_sdk.MetaApi(api_client)

    try:
        # OpenAPI 3.1 specification
        api_response = api_instance.get_open_api_spec()
        print("The response of MetaApi->get_open_api_spec:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling MetaApi->get_open_api_spec: %s\n" % e)
```



### Parameters

This endpoint does not need any parameter.

### Return type

**object**

### Authorization

No authorization required

### HTTP request headers

 - **Content-Type**: Not defined
 - **Accept**: application/json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**200** | OK |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

