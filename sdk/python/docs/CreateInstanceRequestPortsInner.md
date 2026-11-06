# CreateInstanceRequestPortsInner


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**port** | **int** |  | 
**protocol** | **str** |  | 
**primary** | **bool** |  | [optional] 

## Example

```python
from gpuai_sdk.models.create_instance_request_ports_inner import CreateInstanceRequestPortsInner

# TODO update the JSON string below
json = "{}"
# create an instance of CreateInstanceRequestPortsInner from a JSON string
create_instance_request_ports_inner_instance = CreateInstanceRequestPortsInner.from_json(json)
# print the JSON string representation of the object
print(CreateInstanceRequestPortsInner.to_json())

# convert the object into a dict
create_instance_request_ports_inner_dict = create_instance_request_ports_inner_instance.to_dict()
# create an instance of CreateInstanceRequestPortsInner from a dict
create_instance_request_ports_inner_from_dict = CreateInstanceRequestPortsInner.from_dict(create_instance_request_ports_inner_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


