# GPUTypePage


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**data** | [**List[GPUType]**](GPUType.md) |  | 
**next_cursor** | **str** |  | 

## Example

```python
from gpuai_sdk.models.gpu_type_page import GPUTypePage

# TODO update the JSON string below
json = "{}"
# create an instance of GPUTypePage from a JSON string
gpu_type_page_instance = GPUTypePage.from_json(json)
# print the JSON string representation of the object
print(GPUTypePage.to_json())

# convert the object into a dict
gpu_type_page_dict = gpu_type_page_instance.to_dict()
# create an instance of GPUTypePage from a dict
gpu_type_page_from_dict = GPUTypePage.from_dict(gpu_type_page_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


