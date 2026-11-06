
# FrameworkVersion


## Properties

Name | Type
------------ | -------------
`id` | string
`label` | string
`variant` | string
`_default` | boolean

## Example

```typescript
import type { FrameworkVersion } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "id": 2.13,
  "label": PyTorch 2.13,
  "variant": cuda124,
  "_default": null,
} satisfies FrameworkVersion

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as FrameworkVersion
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


