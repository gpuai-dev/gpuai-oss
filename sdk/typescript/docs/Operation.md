
# Operation


## Properties

Name | Type
------------ | -------------
`operationId` | string
`kind` | string
`state` | string
`resourceId` | string
`error` | [OperationError](OperationError.md)
`result` | [OperationResult](OperationResult.md)
`createdAt` | Date
`updatedAt` | Date
`completedAt` | Date
`warnings` | Array&lt;string&gt;

## Example

```typescript
import type { Operation } from '@gpuai/sdk'

// TODO: Update the object below with actual values
const example = {
  "operationId": null,
  "kind": null,
  "state": null,
  "resourceId": null,
  "error": null,
  "result": null,
  "createdAt": null,
  "updatedAt": null,
  "completedAt": null,
  "warnings": null,
} satisfies Operation

console.log(example)

// Convert the instance to a JSON string
const exampleJSON: string = JSON.stringify(example)
console.log(exampleJSON)

// Parse the JSON string back to an object
const exampleParsed = JSON.parse(exampleJSON) as Operation
console.log(exampleParsed)
```

[[Back to top]](#) [[Back to API list]](../README.md#api-endpoints) [[Back to Model list]](../README.md#models) [[Back to README]](../README.md)


