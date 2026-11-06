# RegistryCredential

A stored private-registry login (metadata only — the password is write-only and encrypted at rest).

## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**id** | **str** |  | [optional] 
**name** | **str** |  | [optional] 
**registry** | **str** |  | [optional] 
**username** | **str** |  | [optional] 
**created_at** | **datetime** |  | [optional] 

## Example

```python
from gpuai_sdk.models.registry_credential import RegistryCredential

# TODO update the JSON string below
json = "{}"
# create an instance of RegistryCredential from a JSON string
registry_credential_instance = RegistryCredential.from_json(json)
# print the JSON string representation of the object
print(RegistryCredential.to_json())

# convert the object into a dict
registry_credential_dict = registry_credential_instance.to_dict()
# create an instance of RegistryCredential from a dict
registry_credential_from_dict = RegistryCredential.from_dict(registry_credential_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


