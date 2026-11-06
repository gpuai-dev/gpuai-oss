# gpuai_sdk.RegistryCredentialsApi

All URIs are relative to *http://localhost:8080/v1*

Method | HTTP request | Description
------------- | ------------- | -------------
[**create_registry_credential**](RegistryCredentialsApi.md#create_registry_credential) | **POST** /registry-credentials | Store a registry credential
[**delete_registry_credential**](RegistryCredentialsApi.md#delete_registry_credential) | **DELETE** /registry-credentials/{id} | Delete a registry credential
[**list_registry_credentials**](RegistryCredentialsApi.md#list_registry_credentials) | **GET** /registry-credentials | List registry credentials


# **create_registry_credential**
> RegistryCredential create_registry_credential(create_registry_credential_request)

Store a registry credential

Stores a private-registry login for pulling private custom images. The password crosses the wire exactly once — here — and is encrypted at rest; no read ever returns it. Reference the returned id from `registry_credential_id` on instance creation. The credential's registry host must obey the same rules as image references (no localhost/private addresses); username and password must not contain whitespace. Duplicate names return 409 `name_taken`.

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.create_registry_credential_request import CreateRegistryCredentialRequest
from gpuai_sdk.models.registry_credential import RegistryCredential
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
    api_instance = gpuai_sdk.RegistryCredentialsApi(api_client)
    create_registry_credential_request = gpuai_sdk.CreateRegistryCredentialRequest() # CreateRegistryCredentialRequest | 

    try:
        # Store a registry credential
        api_response = api_instance.create_registry_credential(create_registry_credential_request)
        print("The response of RegistryCredentialsApi->create_registry_credential:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling RegistryCredentialsApi->create_registry_credential: %s\n" % e)
```



### Parameters


Name | Type | Description  | Notes
------------- | ------------- | ------------- | -------------
 **create_registry_credential_request** | [**CreateRegistryCredentialRequest**](CreateRegistryCredentialRequest.md)|  | 

### Return type

[**RegistryCredential**](RegistryCredential.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: application/json
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**201** | Credential stored (metadata only; no password) |  -  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **delete_registry_credential**
> delete_registry_credential(id)

Delete a registry credential

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
    api_instance = gpuai_sdk.RegistryCredentialsApi(api_client)
    id = 'id_example' # str | 

    try:
        # Delete a registry credential
        api_instance.delete_registry_credential(id)
    except Exception as e:
        print("Exception when calling RegistryCredentialsApi->delete_registry_credential: %s\n" % e)
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
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

# **list_registry_credentials**
> ListRegistryCredentials200Response list_registry_credentials()

List registry credentials

Lists the organization's stored private-registry logins for custom-image launches. Passwords are never returned by any read. 503 when the feature is not enabled on this server.

### Example

* Bearer Authentication (bearerAuth):

```python
import gpuai_sdk
from gpuai_sdk.models.list_registry_credentials200_response import ListRegistryCredentials200Response
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
    api_instance = gpuai_sdk.RegistryCredentialsApi(api_client)

    try:
        # List registry credentials
        api_response = api_instance.list_registry_credentials()
        print("The response of RegistryCredentialsApi->list_registry_credentials:\n")
        pprint(api_response)
    except Exception as e:
        print("Exception when calling RegistryCredentialsApi->list_registry_credentials: %s\n" % e)
```



### Parameters

This endpoint does not need any parameter.

### Return type

[**ListRegistryCredentials200Response**](ListRegistryCredentials200Response.md)

### Authorization

[bearerAuth](../README.md#bearerAuth)

### HTTP request headers

 - **Content-Type**: Not defined
 - **Accept**: application/json, application/problem+json

### HTTP response details

| Status code | Description | Response headers |
|-------------|-------------|------------------|
**200** | Credential list |  -  |
**429** | Rate limit exceeded (RFC 7807). Retry-After header indicates seconds to wait. |  * Retry-After - Seconds the client should wait before retrying. <br>  |
**0** | Error response (RFC 7807) |  -  |

[[Back to top]](#) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to Model list]](../README.md#documentation-for-models) [[Back to README]](../README.md)

