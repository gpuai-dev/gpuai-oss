
# CreateInstanceRequest


## Properties

Name | Type
------------ | -------------
`gpuType` | string
`gpuCount` | number
`region` | string
`tier` | string
`sshKeyIds` | Array&lt;string&gt;
`name` | string
`maxPricePerHour` | number
`viewedPricePerHour` | number
`environment` | string
`offeringId` | string
`diskGb` | number
`autoTerminateHours` | number
`env` | { [key: string]: string; }
`image` | string
`registryCredentialId` | string
`entrypoint` | Array&lt;string&gt;
`cmd` | Array&lt;string&gt;
`ports` | [Array&lt;CreateInstanceRequestPortsInner&gt;](CreateInstanceRequestPortsInner.md)

## Example

```typescript
import type { CreateInstanceRequest } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "gpuType": null,
  "gpuCount": null,
  "region": null,
  "tier": null,
  "sshKeyIds": null,
  "name": null,
  "maxPricePerHour": null,
  "viewedPricePerHour": null,
  "environment": certified:pytorch@2.13,
  "offeringId": null,
  "diskGb": 200,
  "autoTerminateHours": 8,
  "env": null,
  "image": ghcr.io/acme/trainer:v1,
  "registryCredentialId": null,
  "entrypoint": null,
  "cmd": null,
  "ports": null,
} satisfies CreateInstanceRequest

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as CreateInstanceRequest
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


