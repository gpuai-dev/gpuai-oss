# UpdateInstanceRequest


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**name** | **str** |  | [optional] 

## Example

```python
from gpuai_sdk.models.update_instance_request import UpdateInstanceRequest

# TODO update the JSON string below
json = "{}"
# create an instance of UpdateInstanceRequest from a JSON string
update_instance_request_instance = UpdateInstanceRequest.from_json(json)
# print the JSON string representation of the object
print(UpdateInstanceRequest.to_json())

# convert the object into a dict
update_instance_request_dict = update_instance_request_instance.to_dict()
# create an instance of UpdateInstanceRequest from a dict
update_instance_request_from_dict = UpdateInstanceRequest.from_dict(update_instance_request_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


