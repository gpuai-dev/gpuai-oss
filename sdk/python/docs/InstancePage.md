# InstancePage


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**data** | [**List[Instance]**](Instance.md) |  | 
**next_cursor** | **str** |  | 

## Example

```python
from gpuai_sdk.models.instance_page import InstancePage

# TODO update the JSON string below
json = "{}"
# create an instance of InstancePage from a JSON string
instance_page_instance = InstancePage.from_json(json)
# print the JSON string representation of the object
print(InstancePage.to_json())

# convert the object into a dict
instance_page_dict = instance_page_instance.to_dict()
# create an instance of InstancePage from a dict
instance_page_from_dict = InstancePage.from_dict(instance_page_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


