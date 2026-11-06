# OperationResult

Terminal-success payload. Carries the per-instance web-access coordinates when the deployment serves them (app_url for an instance with a primary http port, terminal_url for instances provisioned with the browser web console) and the basic-auth username shared by both. Both need the optional tunnel; a deployment without it omits them. The basic-auth PASSWORD is never included here — retrieve it via GET /instances/{id}?include=credentials (connection.app_password).

## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**app_url** | **str** |  | [optional] 
**terminal_url** | **str** |  | [optional] 
**app_basic_auth_user** | **str** |  | [optional] 

## Example

```python
from gpuai_sdk.models.operation_result import OperationResult

# TODO update the JSON string below
json = "{}"
# create an instance of OperationResult from a JSON string
operation_result_instance = OperationResult.from_json(json)
# print the JSON string representation of the object
print(OperationResult.to_json())

# convert the object into a dict
operation_result_dict = operation_result_instance.to_dict()
# create an instance of OperationResult from a dict
operation_result_from_dict = OperationResult.from_dict(operation_result_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


