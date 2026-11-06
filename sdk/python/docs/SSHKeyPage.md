# SSHKeyPage


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**data** | [**List[SSHKey]**](SSHKey.md) |  | 
**next_cursor** | **str** |  | 

## Example

```python
from gpuai_sdk.models.ssh_key_page import SSHKeyPage

# TODO update the JSON string below
json = "{}"
# create an instance of SSHKeyPage from a JSON string
ssh_key_page_instance = SSHKeyPage.from_json(json)
# print the JSON string representation of the object
print(SSHKeyPage.to_json())

# convert the object into a dict
ssh_key_page_dict = ssh_key_page_instance.to_dict()
# create an instance of SSHKeyPage from a dict
ssh_key_page_from_dict = SSHKeyPage.from_dict(ssh_key_page_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


