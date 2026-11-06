# Environments

The launch environment catalog (GET /v1/environments).

## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**frameworks** | **List[str]** | Certified framework ids whose images are fully published — the &#x60;certified:&lt;framework&gt;&#x60; options. | 
**versions** | **Dict[str, List[FrameworkVersion]]** | The version picker roster: framework id → selectable versions (the &#x60;certified:&lt;framework&gt;@&lt;version&gt;&#x60; grammar). A framework absent here has one build per CUDA variant. &#x60;variant&#x60; is the CUDA variant a version targets — only offerings whose GPU maps to it can run it. | 
**includes** | **Dict[str, Dict[str, List[IncludeItem]]]** | \&quot;What&#39;s included\&quot; software lists: framework id (roster ids plus \&quot;base\&quot;) → CUDA variant (\&quot;cuda128\&quot;/\&quot;cuda124\&quot;) → components. | 
**vm_images** | [**Dict[str, VMImage]**](VMImage.md) | The raw-VM OS catalog (the &#x60;raw_vm:&lt;os&gt;&#x60; grammar), keyed by os id. | 

## Example

```python
from gpuai_sdk.models.environments import Environments

# TODO update the JSON string below
json = "{}"
# create an instance of Environments from a JSON string
environments_instance = Environments.from_json(json)
# print the JSON string representation of the object
print(Environments.to_json())

# convert the object into a dict
environments_dict = environments_instance.to_dict()
# create an instance of Environments from a dict
environments_from_dict = Environments.from_dict(environments_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


