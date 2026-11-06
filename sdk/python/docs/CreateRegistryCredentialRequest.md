# CreateRegistryCredentialRequest


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**name** | **str** | Org-unique handle (at most 64 characters). | 
**registry** | **str** | Registry host the login belongs to. | 
**username** | **str** |  | 
**password** | **str** | Registry password or access token. Write-only. | 

## Example

```python
from gpuai_sdk.models.create_registry_credential_request import CreateRegistryCredentialRequest

# TODO update the JSON string below
json = "{}"
# create an instance of CreateRegistryCredentialRequest from a JSON string
create_registry_credential_request_instance = CreateRegistryCredentialRequest.from_json(json)
# print the JSON string representation of the object
print(CreateRegistryCredentialRequest.to_json())

# convert the object into a dict
create_registry_credential_request_dict = create_registry_credential_request_instance.to_dict()
# create an instance of CreateRegistryCredentialRequest from a dict
create_registry_credential_request_from_dict = CreateRegistryCredentialRequest.from_dict(create_registry_credential_request_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


