
# GPUType


## Properties

Name | Type
------------ | -------------
`gpuType` | string
`vramGb` | number
`cpuCores` | number
`ramGb` | number
`storageGb` | number
`instanceDiskGb` | number
`diskConfigurable` | boolean
`architecture` | string

## Example

```typescript
import type { GPUType } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "gpuType": null,
  "vramGb": null,
  "cpuCores": null,
  "ramGb": null,
  "storageGb": null,
  "instanceDiskGb": null,
  "diskConfigurable": null,
  "architecture": null,
} satisfies GPUType

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as GPUType
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


