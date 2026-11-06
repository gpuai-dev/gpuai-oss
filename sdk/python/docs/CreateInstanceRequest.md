# CreateInstanceRequest


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**gpu_type** | **str** |  | 
**gpu_count** | **int** |  | 
**region** | **str** |  | [optional] 
**tier** | **str** | &#x60;spot&#x60; is soft-deprecated: suppliers may not offer spot capacity, in which case tier&#x3D;spot returns no offerings; use on_demand. | 
**ssh_key_ids** | **List[str]** |  | [optional] 
**name** | **str** |  | [optional] 
**max_price_per_hour** | **float** | maximum acceptable whole-instance hourly price (catalog price) | [optional] 
**viewed_price_per_hour** | **float** | The retail $/hr the caller displayed to the user before launch. A catalog row is a quote, not a booking, and pinning an offering fixes which machine is placed but not its rate. If the launch-time price exceeds this by more than $0.01 the launch fails with &#x60;price_changed&#x60; carrying the new price instead of proceeding at the higher price; omit to accept the launch-time price (previous behavior). Refusal only — it is never read as consent, so it can neither raise what you are paid nor widen what may be launched on your behalf. Use max_price_per_hour to bound the price. | [optional] 
**environment** | **str** | Launch environment. Omit for the supplier default (the certified image where supported). Accepted values: &#x60;certified&#x60; (the certified base image), &#x60;certified:&lt;framework&gt;&#x60; (a framework layer — see GET /v1/environments for the roster, e.g. &#x60;certified:pytorch&#x60;), &#x60;certified:&lt;framework&gt;@&lt;version&gt;&#x60; (a specific framework version, e.g. &#x60;certified:pytorch@2.13&#x60;), &#x60;raw_vm&#x60; (root VM, supplier default OS), &#x60;raw_vm:&lt;os&gt;&#x60; (root VM with a chosen OS, e.g. &#x60;raw_vm:ubuntu-24.04&#x60;), or &#x60;provider_template&#x60; (the supplier&#39;s own image). A well-formed value the placed machine can&#39;t serve returns 422 &#x60;environment_unavailable&#x60;. | [optional] 
**offering_id** | **str** | Pin the launch to one exact offering (from GET /v1/pricing) so the engine never substitutes a pricier one. If that offering is gone, the launch fails explicitly rather than silently placing elsewhere. | [optional] 
**disk_gb** | **int** | Requested instance filesystem size in GB. Omit for the platform default (100 GB), which never narrows placement. Setting it is a hard requirement: placement narrows to capacity that can honor it (see &#x60;disk_configurable&#x60; on GET /v1/pricing), and a request no offering of the gpu_type can honor returns 422 &#x60;disk_unavailable&#x60; rather than launching with a different size. When the launch also pins an exact &#x60;offering_id&#x60;, a disk_gb larger than that offering&#39;s displayed &#x60;storage_gb&#x60; returns 422 &#x60;disk_exceeds_offering&#x60; rather than launching with a clamped filesystem. Disk beyond the included allowance (100 GB) is priced at the offering&#39;s &#x60;disk_price_per_gb_hour&#x60; (GET /v1/pricing), folded into the instance&#39;s single hourly rate. Bounds: at least 10, at most the platform ceiling (currently 1000) — outside them returns 422 &#x60;validation_failed&#x60;. | [optional] 
**auto_terminate_hours** | **int** | Terminate this instance automatically after this many hours of runtime. Omit for no limit (the default). The countdown starts when the instance reaches &#x60;running&#x60;, not when you call this — provisioning is not counted against the limit — and the resolved deadline is returned as &#x60;auto_terminate_at&#x60; on every read. Enforcement is server-side and survives your process: this is the safety net for an unattended run whose client dies without cleaning up. When it fires the instance is terminated exactly as a DELETE would terminate it and &#x60;status_reason&#x60; records that the limit was reached. Bounds: at least 1, at most 720 (30 days) — outside them returns 422 &#x60;validation_failed&#x60;. An explicit &#x60;0&#x60; is rejected rather than read as \&quot;no limit\&quot;; omit the field for that. | [optional] 
**env** | **Dict[str, str]** | Environment variables for an &#x60;image&#x60; launch: plain container env vars. Keys must be POSIX names and the platform&#39;s reserved namespace (&#x60;PUBLIC_KEY&#x60;, &#x60;GPUAI_*&#x60;) is refused (422). | [optional] 
**image** | **str** | Launch from your own container image, e.g. &#x60;pytorch/pytorch:2.4.1-cuda12.4-cudnn9-devel&#x60; or &#x60;ghcr.io/acme/trainer:v1&#x60;. The image must be publicly pullable (or private, with &#x60;registry_credential_id&#x60;) and provide linux/amd64 — both are verified synchronously before anything is provisioned: an unknown image returns 422 &#x60;image_unavailable&#x60; and a registry outage 502 &#x60;registry_unreachable&#x60;, with no operation created. The launch is pinned to the exact manifest digest verified, so a tag re-push cannot change what boots. After SSH setup the image&#39;s own ENTRYPOINT/CMD is started (its output is in /var/log/gpuai-entrypoint.log on the instance); an image with neither is a plain SSH environment. Placement prefers container-native capacity (the image runs natively; SSH lands inside it) and falls back to VM capacity (the image runs as a container beside the host&#39;s SSH — inspect with &#x60;docker logs gpuai-workload&#x60;); private images stay container-native. A gpu_type with no capable capacity returns 422 &#x60;image_unavailable&#x60;. Mutually exclusive with &#x60;environment&#x60; (422). A malformed reference (bad charset, localhost/private-IP registry) returns 400. On-demand only for now: &#x60;tier: spot&#x60; with &#x60;image&#x60; is refused (422 &#x60;validation_failed&#x60;) because a preemption re-route cannot yet carry a custom image. | [optional] 
**registry_credential_id** | **str** | Stored registry-credential id (POST /v1/registry-credentials) for pulling a PRIVATE &#x60;image&#x60;. The credential&#39;s registry must match the image&#39;s registry (422 otherwise); wrong logins fail the pre-flight with 422 &#x60;image_unavailable&#x60; before anything is provisioned. Requires &#x60;image&#x60;. | [optional] 
**entrypoint** | **List[str]** | Override the image&#39;s ENTRYPOINT (docker semantics — also drops the image CMD unless &#x60;cmd&#x60; is given). Omit to use the image&#39;s own; an explicit empty array clears it while keeping the image CMD (docker &#x60;--entrypoint \&quot;\&quot;&#x60; semantics). Empty strings inside a non-empty argv are rejected (422). At most 32 arguments / 4KB. Requires &#x60;image&#x60;. | [optional] 
**cmd** | **List[str]** | Override the image&#39;s CMD. Omit to use the image&#39;s own (or none, when &#x60;entrypoint&#x60; is overridden with a non-empty argv; an empty-array entrypoint clear keeps the image CMD). An explicit empty array clears the CMD. Empty strings inside a non-empty argv are rejected (422). Requires &#x60;image&#x60;. | [optional] 
**ports** | [**List[CreateInstanceRequestPortsInner]**](CreateInstanceRequestPortsInner.md) | Container ports to expose on a custom-image launch (at most 10; port 22 is reserved). At most one entry may be &#x60;primary&#x60; — it must be http and is served at the instance&#39;s app URL (&#x60;https://&lt;id&gt;.apps.&lt;your-domain&gt;&#x60;, when the optional tunnel is enabled) behind the per-instance basic-auth login. Requires &#x60;image&#x60;. | [optional] 

## Example

```python
from gpuai_sdk.models.create_instance_request import CreateInstanceRequest

# TODO update the JSON string below
json = "{}"
# create an instance of CreateInstanceRequest from a JSON string
create_instance_request_instance = CreateInstanceRequest.from_json(json)
# print the JSON string representation of the object
print(CreateInstanceRequest.to_json())

# convert the object into a dict
create_instance_request_dict = create_instance_request_instance.to_dict()
# create an instance of CreateInstanceRequest from a dict
create_instance_request_from_dict = CreateInstanceRequest.from_dict(create_instance_request_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


