# GPUType


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**gpu_type** | **str** |  | 
**vram_gb** | **int** |  | 
**cpu_cores** | **int** | Representative host vCPU count for this GPU model. Unlike vram_gb (the card&#39;s nameplate VRAM; a model spanning capacity variants advertises the smallest currently offered, so it is the value every launch of the model can honor) this varies per offering, so it is the first-sighting/typical value; GET /v1/pricing carries the exact per-offering spec. 0 marks a spec the upstream did not report. | [optional] 
**ram_gb** | **int** | Representative host RAM (GB) for this GPU model; varies per offering — see cpu_cores. 0 marks an unreported spec. | [optional] 
**storage_gb** | **int** | Representative host storage (GB) for this GPU model; varies per offering — see cpu_cores. 0 marks an unreported spec. | [optional] 
**instance_disk_gb** | **int** | Representative instance filesystem size (GB) a launch of this model receives — the platform default where the upstream honors a configured size, otherwise the machine&#39;s reported disk; varies per offering — see cpu_cores. 0 marks an unreported disk (unknown, not zero). GET /v1/pricing carries the exact per-offering value. | [optional] 
**disk_configurable** | **bool** | true when at least one offering of this model accepts a custom disk_gb at launch (an OR across offerings — requesting disk_gb narrows placement to the capable capacity). | [optional] 
**architecture** | **str** |  | [optional] 

## Example

```python
from gpuai_sdk.models.gpu_type import GPUType

# TODO update the JSON string below
json = "{}"
# create an instance of GPUType from a JSON string
gpu_type_instance = GPUType.from_json(json)
# print the JSON string representation of the object
print(GPUType.to_json())

# convert the object into a dict
gpu_type_dict = gpu_type_instance.to_dict()
# create an instance of GPUType from a dict
gpu_type_from_dict = GPUType.from_dict(gpu_type_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


