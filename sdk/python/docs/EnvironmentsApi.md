# gpuai_sdk.EnvironmentsApi

All URIs are relative to *http://localhost:8080/v1*

Method | HTTP request | Description
------------- | ------------- | -------------
[**get_environments**](EnvironmentsApi.md#get_environments) | **GET** /environments | List launch environments (no auth required)


# **get_environments**
> Environments get_environments()

List launch environments (no auth required)

The server-controlled launch environment catalog — the certified framework roster, the selectable version(s) per framework (the version picker), the "What's included" software lists, and the raw-VM OS catalog. These are exactly the values accepted by the `environment` field of POST /v1/instances.

### Example


```python
import gpuai_sdk
from gpuai_sdk.models.environments import Environments
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
    api_instance = gpuai_sdk.EnvironmentsApi(api_client)

    try:
        # List launch environments (no auth required)
        api_response = api_instance.get_environments()
        print("The response of EnvironmentsApi->get_environments:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling EnvironmentsApi->get_environments: %s\n" % e)
```



### Parameters

This endpoint does not need any parameter.

### Return type

[**Environments**](Environments.md)

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

