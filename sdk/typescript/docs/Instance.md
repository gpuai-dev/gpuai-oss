
# Instance


## Properties

Name | Type
------------ | -------------
`id` | string
`name` | string
`status` | string
`gpuType` | string
`gpuCount` | number
`region` | string
`tier` | string
`pricePerHour` | number
`connection` | [InstanceConnection](InstanceConnection.md)
`diskGb` | number
`image` | string
`autoTerminateHours` | number
`autoTerminateAt` | Date
`statusReason` | string
`createdAt` | Date
`readyAt` | Date
`terminatedAt` | Date
`lastReachableAt` | Date
`unreachableSince` | Date

## Example

```typescript
import type { Instance } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "id": null,
  "name": null,
  "status": null,
  "gpuType": null,
  "gpuCount": null,
  "region": null,
  "tier": null,
  "pricePerHour": null,
  "connection": null,
  "diskGb": null,
  "image": null,
  "autoTerminateHours": 8,
  "autoTerminateAt": null,
  "statusReason": null,
  "createdAt": null,
  "readyAt": null,
  "terminatedAt": null,
  "lastReachableAt": null,
  "unreachableSince": null,
} satisfies Instance

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as Instance
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


