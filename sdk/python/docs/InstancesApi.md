# gpuai_sdk.InstancesApi

All URIs are relative to *http://localhost:8080/v1*

Method | HTTP request | Description
------------- | ------------- | -------------
[**create_instance**](InstancesApi.md#create_instance) | **POST** /instances | Create an instance (async — returns 202 + Operation-Id header)
[**delete_instance**](InstancesApi.md#delete_instance) | **DELETE** /instances/{id} | Terminate an instance (idempotent)
[**get_instance**](InstancesApi.md#get_instance) | **GET** /instances/{id} | Get an instance
[**list_instances**](InstancesApi.md#list_instances) | **GET** /instances | List instances
[**update_instance**](InstancesApi.md#update_instance) | **PATCH** /instances/{id} | Update an instance (rename only in v1)


# **create_instance**
> Operation create_instance(create_instance_request, idempotency_key=idempotency_key)

Create an instance (async — returns 202 + Operation-Id header)

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.create_instance_request import CreateInstanceRequest
from gpuai_sdk.models.operation import Operation
from gpuai_sdk.rest import ApiException
from pprint import pprint

# Defining the host is optional and defaults to http://localhost:8080/v1
# See configuration.py for a list of all supported configuration parameters.
configuration = gpuai_sdk.Configuration(
    host = "http://localhost:8080/v1"
)

# The client must configure the authentication and authorization parameters
# in accordance with the API server security policy.
# Examples for each auth method are provided below, use the example that
# satisfies your auth use case.

# Configure Bearer authorization: bearerAuth
configuration = gpuai_sdk.Configuration(
    access_token = os.environ["BEARER_TOKEN"]
)

# Enter a context with an instance of the API client
with gpuai_sdk.ApiClient(configuration) as api_client:
    # Create an instance of the API class
    api_instance = gpuai_sdk.InstancesApi(api_client)
    create_instance_request = gpuai_sdk.CreateInstanceRequest() # CreateInstanceRequest | 
    idempotency_key = 'idempotency_key_example' # str |  (optional)

    try:
        # Create an instance (async — returns 202 + Operation-Id header)
        api_response = api_instance.create_instance(create_instance_request, idempotency_key=idempotency_key)
        print("The response of InstancesApi->create_instance:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling InstancesApi->create_instance: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **create_instance_request** | [**CreateInstanceRequest**](CreateInstanceRequest.md)|  | 
 **idempotency_key** | **str**|  | [optional] 

### Return type

[**Operation**](Operation.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: application/json
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**202** | Accepted; long-running operation |  * Operation-Id -  <br>  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **delete_instance**
> Operation delete_instance(id, idempotency_key=idempotency_key)

Terminate an instance (idempotent)

Members can terminate only instances they created; organization admins can terminate any instance in the organization. A self-hosted deployment's API keys belong to the default organization's admin.

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.operation import Operation
from gpuai_sdk.rest import ApiException
from pprint import pprint

# Defining the host is optional and defaults to http://localhost:8080/v1
# See configuration.py for a list of all supported configuration parameters.
configuration = gpuai_sdk.Configuration(
    host = "http://localhost:8080/v1"
)

# The client must configure the authentication and authorization parameters
# in accordance with the API server security policy.
# Examples for each auth method are provided below, use the example that
# satisfies your auth use case.

# Configure Bearer authorization: bearerAuth
configuration = gpuai_sdk.Configuration(
    access_token = os.environ["BEARER_TOKEN"]
)

# Enter a context with an instance of the API client
with gpuai_sdk.ApiClient(configuration) as api_client:
    # Create an instance of the API class
    api_instance = gpuai_sdk.InstancesApi(api_client)
    id = 'id_example' # str | 
    idempotency_key = 'idempotency_key_example' # str |  (optional)

    try:
        # Terminate an instance (idempotent)
        api_response = api_instance.delete_instance(id, idempotency_key=idempotency_key)
        print("The response of InstancesApi->delete_instance:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling InstancesApi->delete_instance: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **id** | **str**|  | 
 **idempotency_key** | **str**|  | [optional] 

### Return type

[**Operation**](Operation.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: Not defined
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**202** | Accepted; termination in progress |  * Operation-Id -  <br>  |
**403** | Forbidden — members can only terminate instances they created |  -  |
**404** | Already gone (idempotent terminate) |  -  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **get_instance**
> Instance get_instance(id, include=include)

Get an instance

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.instance import Instance
from gpuai_sdk.rest import ApiException
from pprint import pprint

# Defining the host is optional and defaults to http://localhost:8080/v1
# See configuration.py for a list of all supported configuration parameters.
configuration = gpuai_sdk.Configuration(
    host = "http://localhost:8080/v1"
)

# The client must configure the authentication and authorization parameters
# in accordance with the API server security policy.
# Examples for each auth method are provided below, use the example that
# satisfies your auth use case.

# Configure Bearer authorization: bearerAuth
configuration = gpuai_sdk.Configuration(
    access_token = os.environ["BEARER_TOKEN"]
)

# Enter a context with an instance of the API client
with gpuai_sdk.ApiClient(configuration) as api_client:
    # Create an instance of the API class
    api_instance = gpuai_sdk.InstancesApi(api_client)
    id = 'id_example' # str | 
    include = 'include_example' # str | Opt-in expansion. `credentials` restores connection.app_password on this single-instance read — the only surface that returns the web-console basic-auth password. Omitted by default so bare reads can be logged without leaking the secret. Any other value is a 400 validation_failed. The list endpoint and operation reads never return the password, with or without this parameter. (optional)

    try:
        # Get an instance
        api_response = api_instance.get_instance(id, include=include)
        print("The response of InstancesApi->get_instance:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling InstancesApi->get_instance: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **id** | **str**|  | 
 **include** | **str**| Opt-in expansion. &#x60;credentials&#x60; restores connection.app_password on this single-instance read — the only surface that returns the web-console basic-auth password. Omitted by default so bare reads can be logged without leaking the secret. Any other value is a 400 validation_failed. The list endpoint and operation reads never return the password, with or without this parameter. | [optional] 

### Return type

[**Instance**](Instance.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: Not defined
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**200** | OK |  -  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **list_instances**
> InstancePage list_instances(cursor=cursor, limit=limit, status=status)

List instances

Lists the organization's instances. By default terminated instances are excluded; pass status=terminated for the history or status=all for everything.

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.instance_page import InstancePage
from gpuai_sdk.rest import ApiException
from pprint import pprint

# Defining the host is optional and defaults to http://localhost:8080/v1
# See configuration.py for a list of all supported configuration parameters.
configuration = gpuai_sdk.Configuration(
    host = "http://localhost:8080/v1"
)

# The client must configure the authentication and authorization parameters
# in accordance with the API server security policy.
# Examples for each auth method are provided below, use the example that
# satisfies your auth use case.

# Configure Bearer authorization: bearerAuth
configuration = gpuai_sdk.Configuration(
    access_token = os.environ["BEARER_TOKEN"]
)

# Enter a context with an instance of the API client
with gpuai_sdk.ApiClient(configuration) as api_client:
    # Create an instance of the API class
    api_instance = gpuai_sdk.InstancesApi(api_client)
    cursor = 'cursor_example' # str |  (optional)
    limit = 50 # int |  (optional) (default to 50)
    status = 'status_example' # str | Filter by status (default = all non-terminated). There is no \"unreachable\" filter value: an unreachable instance is a running instance whose SSH path is down, so it is listed under status=running and reads \"unreachable\" in the response body. (optional)

    try:
        # List instances
        api_response = api_instance.list_instances(cursor=cursor, limit=limit, status=status)
        print("The response of InstancesApi->list_instances:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling InstancesApi->list_instances: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **cursor** | **str**|  | [optional] 
 **limit** | **int**|  | [optional] [default to 50]
 **status** | **str**| Filter by status (default &#x3D; all non-terminated). There is no \&quot;unreachable\&quot; filter value: an unreachable instance is a running instance whose SSH path is down, so it is listed under status&#x3D;running and reads \&quot;unreachable\&quot; in the response body. | [optional] 

### Return type

[**InstancePage**](InstancePage.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: Not defined
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**200** | OK |  -  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **update_instance**
> Instance update_instance(id, update_instance_request, idempotency_key=idempotency_key)

Update an instance (rename only in v1)

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.instance import Instance
from gpuai_sdk.models.update_instance_request import UpdateInstanceRequest
from gpuai_sdk.rest import ApiException
from pprint import pprint

# Defining the host is optional and defaults to http://localhost:8080/v1
# See configuration.py for a list of all supported configuration parameters.
configuration = gpuai_sdk.Configuration(
    host = "http://localhost:8080/v1"
)

# The client must configure the authentication and authorization parameters
# in accordance with the API server security policy.
# Examples for each auth method are provided below, use the example that
# satisfies your auth use case.

# Configure Bearer authorization: bearerAuth
configuration = gpuai_sdk.Configuration(
    access_token = os.environ["BEARER_TOKEN"]
)

# Enter a context with an instance of the API client
with gpuai_sdk.ApiClient(configuration) as api_client:
    # Create an instance of the API class
    api_instance = gpuai_sdk.InstancesApi(api_client)
    id = 'id_example' # str | 
    update_instance_request = gpuai_sdk.UpdateInstanceRequest() # UpdateInstanceRequest | 
    idempotency_key = 'idempotency_key_example' # str |  (optional)

    try:
        # Update an instance (rename only in v1)
        api_response = api_instance.update_instance(id, update_instance_request, idempotency_key=idempotency_key)
        print("The response of InstancesApi->update_instance:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling InstancesApi->update_instance: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **id** | **str**|  | 
 **update_instance_request** | [**UpdateInstanceRequest**](UpdateInstanceRequest.md)|  | 
 **idempotency_key** | **str**|  | [optional] 

### Return type

[**Instance**](Instance.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: application/json
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**200** | OK |  -  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

