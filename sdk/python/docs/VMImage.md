# VMImage


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**label** | **str** |  | 
**family** | **str** |  | 
**version** | **str** |  | 
**driver_preinstalled** | **bool** | true when the image ships the NVIDIA driver + CUDA baked in. | [optional] 

## Example

```python
from gpuai_sdk.models.vm_image import VMImage

# TODO update the JSON string below
json = "{}"
# create an instance of VMImage from a JSON string
vm_image_instance = VMImage.from_json(json)
# print the JSON string representation of the object
print(VMImage.to_json())

# convert the object into a dict
vm_image_dict = vm_image_instance.to_dict()
# create an instance of VMImage from a dict
vm_image_from_dict = VMImage.from_dict(vm_image_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


