# Instance


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**id** | **str** |  | 
**name** | **str** |  | [optional] 
**status** | **str** | Lifecycle status. \&quot;allocating\&quot; &#x3D; the GPU is still being acquired from the supplier (slow-boot capacity such as bare metal can take ~15 min); \&quot;starting\&quot; &#x3D; the node exists and is booting; \&quot;running\&quot; &#x3D; ready to use. \&quot;unreachable\&quot; &#x3D; the platform&#39;s probe has confirmed the SSH path is down while the machine may still be up: the instance is NOT usable right now, and it either recovers to \&quot;running\&quot; on its own or is failed after a grace window. It is a degraded reading of a running instance, not a separate lifecycle state; see unreachable_since. | 
**gpu_type** | **str** |  | 
**gpu_count** | **int** |  | 
**region** | **str** |  | 
**tier** | **str** | &#x60;spot&#x60; is soft-deprecated: suppliers may not offer spot capacity, in which case tier&#x3D;spot returns no offerings; use on_demand. | 
**price_per_hour** | **float** | hourly rate for the whole instance — covers all gpu_count GPUs | 
**connection** | [**InstanceConnection**](InstanceConnection.md) |  | [optional] 
**disk_gb** | **int** | Instance filesystem size in GB that the platform configured for this launch. Present only where the placed capacity honors a configured disk size; absent means the machine&#39;s disk is supplier-determined (its own flavor volume). | [optional] 
**image** | **str** | The customer-supplied container image this instance was launched from, digest-pinned at verification time (e.g. &#x60;ghcr.io/acme/trainer:v1@sha256:…&#x60;). Present only on custom-image launches; certified launches never expose their images here. | [optional] 
**auto_terminate_hours** | **int** | The runtime limit this instance was launched with, in hours (the &#x60;auto_terminate_hours&#x60; you sent). Absent means the instance runs until something stops it. It stays present after the limit has fired, so a terminated instance still explains itself. | [optional] 
**auto_terminate_at** | **datetime** | When the runtime limit expires and the platform terminates this instance. Computed when the instance became running (ready_at + auto_terminate_hours), so the countdown covers usable runtime, not the time spent provisioning. Absent when no limit was set, when the instance is not running yet, or once the platform has begun acting on an elapsed limit. | [optional] 
**status_reason** | **str** | Why the instance stopped or failed, e.g. \&quot;provisioning stalled: the instance started but never became reachable\&quot;, or — when a runtime limit fired — \&quot;Terminated: this instance reached the 8-hour runtime limit set at launch\&quot;. Present only when the platform has something to explain. | [optional] 
**created_at** | **datetime** |  | 
**ready_at** | **datetime** |  | [optional] 
**terminated_at** | **datetime** |  | [optional] 
**last_reachable_at** | **datetime** | Last time the platform verified the SSH path to this instance end-to-end. Absent means the instance has not been probed yet (freshly launched, or no probe configured); it does NOT mean unreachable. An instance whose status is still \&quot;running\&quot; while this timestamp stops advancing is one whose SSH has died even though the machine is up. | [optional] 
**unreachable_since** | **datetime** | Start of the confirmed SSH outage the instance is currently in. Present exactly when status is \&quot;unreachable\&quot;, absent otherwise. | [optional] 

## Example

```python
from gpuai_sdk.models.instance import Instance

# TODO update the JSON string below
json = "{}"
# create an instance of Instance from a JSON string
instance_instance = Instance.from_json(json)
# print the JSON string representation of the object
print(Instance.to_json())

# convert the object into a dict
instance_dict = instance_instance.to_dict()
# create an instance of Instance from a dict
instance_from_dict = Instance.from_dict(instance_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


