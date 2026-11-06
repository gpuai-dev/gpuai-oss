# SSHKey


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**id** | **str** |  | 
**name** | **str** |  | 
**fingerprint** | **str** |  | 
**created_at** | **datetime** |  | 

## Example

```python
from gpuai_sdk.models.ssh_key import SSHKey

# TODO update the JSON string below
json = "{}"
# create an instance of SSHKey from a JSON string
ssh_key_instance = SSHKey.from_json(json)
# print the JSON string representation of the object
print(SSHKey.to_json())

# convert the object into a dict
ssh_key_dict = ssh_key_instance.to_dict()
# create an instance of SSHKey from a dict
ssh_key_from_dict = SSHKey.from_dict(ssh_key_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


