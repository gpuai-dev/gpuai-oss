
# Pricing


## Properties

Name | Type
------------ | -------------
`gpuType` | string
`gpuCount` | number
`cpuCores` | number
`ramGb` | number
`storageGb` | number
`region` | string
`tier` | string
`pricePerHour` | number
`available` | number
`instantBoot` | boolean
`community` | boolean
`instanceDiskGb` | number
`diskConfigurable` | boolean
`diskPricePerGbHour` | number
`capacityClass` | string

## Example

```typescript
import type { Pricing } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "gpuType": null,
  "gpuCount": null,
  "cpuCores": null,
  "ramGb": null,
  "storageGb": null,
  "region": null,
  "tier": null,
  "pricePerHour": null,
  "available": null,
  "instantBoot": null,
  "community": null,
  "instanceDiskGb": null,
  "diskConfigurable": null,
  "diskPricePerGbHour": null,
  "capacityClass": null,
} satisfies Pricing

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as Pricing
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


