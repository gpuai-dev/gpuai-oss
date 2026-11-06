# CreateSshKeyRequest


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**name** | **str** |  | 
**public_key** | **str** |  | 

## Example

```python
from gpuai_sdk.models.create_ssh_key_request import CreateSshKeyRequest

# TODO update the JSON string below
json = "{}"
# create an instance of CreateSshKeyRequest from a JSON string
create_ssh_key_request_instance = CreateSshKeyRequest.from_json(json)
# print the JSON string representation of the object
print(CreateSshKeyRequest.to_json())

# convert the object into a dict
create_ssh_key_request_dict = create_ssh_key_request_instance.to_dict()
# create an instance of CreateSshKeyRequest from a dict
create_ssh_key_request_from_dict = CreateSshKeyRequest.from_dict(create_ssh_key_request_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


