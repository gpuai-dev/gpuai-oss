# InstanceConnection


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**hostname** | **str** |  | [optional] 
**port** | **int** |  | [optional] 
**ssh_command** | **str** |  | [optional] 
**app_url** | **str** | HTTPS URL of the instance&#39;s primary http port, e.g. https://gpu-abcd1234.apps.example.com. Present only when the deployment runs the optional tunnel. | [optional] 
**app_user** | **str** | HTTP Basic Auth username for app_url and terminal_url. Present when app_url or terminal_url is. | [optional] 
**app_password** | **str** | HTTP Basic Auth password for app_url and terminal_url. Treat as a secret. OMITTED by default from all reads — returned only on GET /instances/{id}?include&#x3D;credentials (same persistence availability as app_user); the list endpoint and operation results never include it. | [optional] 
**terminal_url** | **str** | HTTPS URL of the browser web console (a shell on the instance, no SSH key required), e.g. https://gpu-abcd1234-term.apps.example.com. Guarded by the same app_user/app_password login. Present only on running instances provisioned with the console, which needs the optional tunnel. | [optional] 

## Example

```python
from gpuai_sdk.models.instance_connection import InstanceConnection

# TODO update the JSON string below
json = "{}"
# create an instance of InstanceConnection from a JSON string
instance_connection_instance = InstanceConnection.from_json(json)
# print the JSON string representation of the object
print(InstanceConnection.to_json())

# convert the object into a dict
instance_connection_dict = instance_connection_instance.to_dict()
# create an instance of InstanceConnection from a dict
instance_connection_from_dict = InstanceConnection.from_dict(instance_connection_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


