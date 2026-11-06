# Operation


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**operation_id** | **UUID** |  | 
**kind** | **str** |  | 
**state** | **str** |  | 
**resource_id** | **str** |  | [optional] 
**error** | [**OperationError**](OperationError.md) |  | [optional] 
**result** | [**OperationResult**](OperationResult.md) |  | [optional] 
**created_at** | **datetime** |  | 
**updated_at** | **datetime** |  | [optional] 
**completed_at** | **datetime** |  | [optional] 
**warnings** | **List[str]** | Create-time advisories, present only on the synchronous 202 from POST /instances (never on GET /operations/{id} polls). | [optional] 

## Example

```python
from gpuai_sdk.models.operation import Operation

# TODO update the JSON string below
json = "{}"
# create an instance of Operation from a JSON string
operation_instance = Operation.from_json(json)
# print the JSON string representation of the object
print(Operation.to_json())

# convert the object into a dict
operation_dict = operation_instance.to_dict()
# create an instance of Operation from a dict
operation_from_dict = Operation.from_dict(operation_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


