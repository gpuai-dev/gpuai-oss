# FrameworkVersion


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**id** | **str** | The version token used in &#x60;certified:&lt;framework&gt;@&lt;id&gt;&#x60;. | 
**label** | **str** |  | 
**variant** | **str** | CUDA variant this version targets (cuda128/cuda124). | 
**default** | **bool** | Preselected in the picker (at most one per framework). | [optional] 

## Example

```python
from gpuai_sdk.models.framework_version import FrameworkVersion

# TODO update the JSON string below
json = "{}"
# create an instance of FrameworkVersion from a JSON string
framework_version_instance = FrameworkVersion.from_json(json)
# print the JSON string representation of the object
print(FrameworkVersion.to_json())

# convert the object into a dict
framework_version_dict = framework_version_instance.to_dict()
# create an instance of FrameworkVersion from a dict
framework_version_from_dict = FrameworkVersion.from_dict(framework_version_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


