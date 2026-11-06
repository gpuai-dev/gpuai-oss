# gpuai_sdk.SshKeysApi

All URIs are relative to *http://localhost:8080/v1*

Method | HTTP request | Description
------------- | ------------- | -------------
[**create_ssh_key**](SshKeysApi.md#create_ssh_key) | **POST** /ssh-keys | Create an SSH key
[**delete_ssh_key**](SshKeysApi.md#delete_ssh_key) | **DELETE** /ssh-keys/{id} | Delete an SSH key (idempotent; 404 when already gone)
[**get_ssh_key**](SshKeysApi.md#get_ssh_key) | **GET** /ssh-keys/{id} | Get an SSH key
[**list_ssh_keys**](SshKeysApi.md#list_ssh_keys) | **GET** /ssh-keys | List SSH keys


# **create_ssh_key**
> SSHKey create_ssh_key(create_ssh_key_request, idempotency_key=idempotency_key)

Create an SSH key

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.create_ssh_key_request import CreateSshKeyRequest
from gpuai_sdk.models.ssh_key import SSHKey
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
    api_instance = gpuai_sdk.SshKeysApi(api_client)
    create_ssh_key_request = gpuai_sdk.CreateSshKeyRequest() # CreateSshKeyRequest | 
    idempotency_key = 'idempotency_key_example' # str |  (optional)

    try:
        # Create an SSH key
        api_response = api_instance.create_ssh_key(create_ssh_key_request, idempotency_key=idempotency_key)
        print("The response of SshKeysApi->create_ssh_key:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling SshKeysApi->create_ssh_key: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **create_ssh_key_request** | [**CreateSshKeyRequest**](CreateSshKeyRequest.md)|  | 
 **idempotency_key** | **str**|  | [optional] 

### Return type

[**SSHKey**](SSHKey.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: application/json
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**201** | Created |  -  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **delete_ssh_key**
> delete_ssh_key(id)

Delete an SSH key (idempotent; 404 when already gone)

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
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
    api_instance = gpuai_sdk.SshKeysApi(api_client)
    id = 'id_example' # str | 

    try:
        # Delete an SSH key (idempotent; 404 when already gone)
        api_instance.delete_ssh_key(id)
    except Exception as e:
        print("Exception when calling SshKeysApi->delete_ssh_key: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **id** | **str**|  | 

### Return type

void (empty response body)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: Not defined
 - **Accept**: application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**204** | Deleted |  -  |
**404** | Already deleted (idempotent) |  -  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **get_ssh_key**
> SSHKey get_ssh_key(id)

Get an SSH key

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.ssh_key import SSHKey
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
    api_instance = gpuai_sdk.SshKeysApi(api_client)
    id = 'id_example' # str | 

    try:
        # Get an SSH key
        api_response = api_instance.get_ssh_key(id)
        print("The response of SshKeysApi->get_ssh_key:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling SshKeysApi->get_ssh_key: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **id** | **str**|  | 

### Return type

[**SSHKey**](SSHKey.md)

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

# **list_ssh_keys**
> SSHKeyPage list_ssh_keys(cursor=cursor, limit=limit)

List SSH keys

Returns the SSH keys owned by the user the API key belongs to.

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.ssh_key_page import SSHKeyPage
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
    api_instance = gpuai_sdk.SshKeysApi(api_client)
    cursor = 'cursor_example' # str |  (optional)
    limit = 50 # int |  (optional) (default to 50)

    try:
        # List SSH keys
        api_response = api_instance.list_ssh_keys(cursor=cursor, limit=limit)
        print("The response of SshKeysApi->list_ssh_keys:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling SshKeysApi->list_ssh_keys: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **cursor** | **str**|  | [optional] 
 **limit** | **int**|  | [optional] [default to 50]

### Return type

[**SSHKeyPage**](SSHKeyPage.md)

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

