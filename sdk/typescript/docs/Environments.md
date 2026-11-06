
# Environments

The launch environment catalog (GET /v1/environments).

## Properties

Name | Type
------------ | -------------
`frameworks` | Array&lt;string&gt;
`versions` | { [key: string]: Array&lt;FrameworkVersion&gt;; }
`includes` | { [key: string]: { [key: string]: Array&lt;IncludeItem&gt;; }; }
`vmImages` | [{ [key: string]: VMImage; }](VMImage.md)

## Example

```typescript
import type { Environments } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "frameworks": [pytorch, cuda-devel, jupyter],
  "versions": null,
  "includes": null,
  "vmImages": null,
} satisfies Environments

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as Environments
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


