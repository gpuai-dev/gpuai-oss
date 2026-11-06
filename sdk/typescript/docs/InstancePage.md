
# InstancePage


## Properties

Name | Type
------------ | -------------
`data` | [Array&lt;Instance&gt;](Instance.md)
`nextCursor` | string

## Example

```typescript
import type { InstancePage } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "data": null,
  "nextCursor": null,
} satisfies InstancePage

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as InstancePage
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


