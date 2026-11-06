
# PricingPage


## Properties

Name | Type
------------ | -------------
`data` | [Array&lt;Pricing&gt;](Pricing.md)
`nextCursor` | string

## Example

```typescript
import type { PricingPage } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "data": null,
  "nextCursor": null,
} satisfies PricingPage

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as PricingPage
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


