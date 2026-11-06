# Pricing


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**gpu_type** | **str** |  | 
**gpu_count** | **int** | number of GPUs in the listed configuration | 
**cpu_cores** | **int** | host vCPU count for this offering, tied to the shown (cheapest) price. 0 marks an unreported spec. | [optional] 
**ram_gb** | **int** | host RAM (GB) for this offering, tied to the shown price. 0 marks an unreported spec. | [optional] 
**storage_gb** | **int** | host storage (GB) for this offering, tied to the shown price. 0 marks an unreported spec. | [optional] 
**region** | **str** |  | 
**tier** | **str** | &#x60;spot&#x60; is soft-deprecated: suppliers may not offer spot capacity, in which case tier&#x3D;spot returns no offerings; use on_demand. | 
**price_per_hour** | **float** | hourly price for the whole listed configuration — covers all gpu_count GPUs | 
**available** | **int** | count of available units, summed across merged identical offers | 
**instant_boot** | **bool** | true when instances of this offering are typically reachable within about a minute of launch; false for capacity with longer provisioning times (~3-5 minutes) | 
**community** | **bool** | true when this offering is supplier/peer-hosted capacity with a softer SLA (see capacity_class) | 
**instance_disk_gb** | **int** | Instance filesystem size (GB) a launch of THIS offering receives — the platform default where the upstream honors a configured size, otherwise the machine&#39;s own reported disk; tied to the shown (cheapest) price like cpu_cores. 0 marks an unreported disk (unknown, not zero). | [optional] 
**disk_configurable** | **bool** | true when a launch of this offering may choose its own disk size via the create request&#39;s disk_gb. A distinguishing field in the offer merge — configurable and fixed-disk capacity for the same SKU stay separate rows. | [optional] 
**disk_price_per_gb_hour** | **float** | Price of one GB of instance disk beyond instance_disk_gb, per hour, for launches of this offering. A launch requesting disk_gb above instance_disk_gb is priced approximately price_per_hour + (disk_gb − instance_disk_gb) × disk_price_per_gb_hour (the exact rate folds disk into one cent-rounded hourly price). 0 when the offering is not disk-configurable, or when disk above the included allowance is currently free. | [optional] 
**capacity_class** | **str** | per-offering capacity classification — \&quot;secure\&quot; for datacenter-operated hardware, \&quot;community\&quot; for supplier/peer-hosted hardware with a softer SLA. The string twin of &#x60;community&#x60;, classified per offering. May transiently be empty for cache entries written before the field existed. | [optional] 

## Example

```python
from gpuai_sdk.models.pricing import Pricing

# TODO update the JSON string below
json = "{}"
# create an instance of Pricing from a JSON string
pricing_instance = Pricing.from_json(json)
# print the JSON string representation of the object
print(Pricing.to_json())

# convert the object into a dict
pricing_dict = pricing_instance.to_dict()
# create an instance of Pricing from a dict
pricing_from_dict = Pricing.from_dict(pricing_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


