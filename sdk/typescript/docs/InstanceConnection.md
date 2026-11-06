
# InstanceConnection


## Properties

Name | Type
------------ | -------------
`hostname` | string
`port` | number
`sshCommand` | string
`appUrl` | string
`appUser` | string
`appPassword` | string
`terminalUrl` | string

## Example

```typescript
import type { InstanceConnection } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "hostname": null,
  "port": null,
  "sshCommand": null,
  "appUrl": null,
  "appUser": null,
  "appPassword": null,
  "terminalUrl": null,
} satisfies InstanceConnection

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as InstanceConnection
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


