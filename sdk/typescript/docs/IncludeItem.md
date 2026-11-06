
# IncludeItem


## Properties

Name | Type
------------ | -------------
`name` | string
`version` | string

## Example

```typescript
import type { IncludeItem } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "name": PyTorch,
  "version": 2.13.0,
} satisfies IncludeItem

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as IncludeItem
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


