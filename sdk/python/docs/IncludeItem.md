# IncludeItem


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**name** | **str** |  | 
**version** | **str** | Resolved build version; absent for unpinned components (e.g. git). | [optional] 

## Example

```python
from gpuai_sdk.models.include_item import IncludeItem

# TODO update the JSON string below
json = "{}"
# create an instance of IncludeItem from a JSON string
include_item_instance = IncludeItem.from_json(json)
# print the JSON string representation of the object
print(IncludeItem.to_json())

# convert the object into a dict
include_item_dict = include_item_instance.to_dict()
# create an instance of IncludeItem from a dict
include_item_from_dict = IncludeItem.from_dict(include_item_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


