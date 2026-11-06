# PricingPage


## Properties

Name | Type | Description | Notes
------------ | ------------- | ------------- | -------------
**data** | [**List[Pricing]**](Pricing.md) |  | 
**next_cursor** | **str** |  | 

## Example

```python
from gpuai_sdk.models.pricing_page import PricingPage

# TODO update the JSON string below
json = "{}"
# create an instance of PricingPage from a JSON string
pricing_page_instance = PricingPage.from_json(json)
# print the JSON string representation of the object
print(PricingPage.to_json())

# convert the object into a dict
pricing_page_dict = pricing_page_instance.to_dict()
# create an instance of PricingPage from a dict
pricing_page_from_dict = PricingPage.from_dict(pricing_page_dict)
```
[[Back to Model list]](../README.md#documentation-for-models) [[Back to API list]](../README.md#documentation-for-api-endpoints) [[Back to README]](../README.md)


