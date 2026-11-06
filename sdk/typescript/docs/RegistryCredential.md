
# RegistryCredential

A stored private-registry login (metadata only — the password is write-only and encrypted at rest).

## Properties

Name | Type
------------ | -------------
`id` | string
`name` | string
`registry` | string
`username` | string
`createdAt` | Date

## Example

```typescript
import type { RegistryCredential } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "id": null,
  "name": ghcr-acme,
  "registry": ghcr.io,
  "username": null,
  "createdAt": null,
} satisfies RegistryCredential

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as RegistryCredential
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


