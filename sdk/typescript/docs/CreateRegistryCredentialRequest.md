
# CreateRegistryCredentialRequest


## Properties

Name | Type
------------ | -------------
`name` | string
`registry` | string
`username` | string
`password` | string

## Example

```typescript
import type { CreateRegistryCredentialRequest } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "name": ghcr-acme,
  "registry": ghcr.io,
  "username": null,
  "password": null,
} satisfies CreateRegistryCredentialRequest

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as CreateRegistryCredentialRequest
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


