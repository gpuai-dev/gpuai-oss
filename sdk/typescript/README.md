# @gpuai/sdk@0.1.0

TypeScript SDK for the GPU.ai Open Source API.

## Usage

The package is not published to a registry. Build it from a checkout of this
repository and install it from the local path:

```bash
cd sdk/typescript && npm install && npm run build
cd /path/to/your/project && npm install /path/to/gpuai-oss/sdk/typescript
```

The client talks to a self-hosted gpuctl server. The base URL comes from the
`basePath` option, else the `GPUAI_API_BASE` environment variable (Node only),
else `http://localhost:8080/v1`. Create an API key on the server with
`gpuctl apikey create --name <name>`.

```ts
import { Configuration, InstancesApi } from '@gpuai/sdk';

const config = new Configuration({
  basePath: process.env.GPUAI_API_BASE ?? 'http://localhost:8080/v1',
  accessToken: process.env.GPUAI_API_KEY,
});

const instances = await new InstancesApi(config).listInstances();
console.log(instances.data);
```


## Documentation

### API Endpoints

All URIs are relative to *http://localhost:8080/v1*

| Class | Method | HTTP request | Description
| ----- | ------ | ------------ | -------------
*EnvironmentsApi* | [**getEnvironments**](docs/EnvironmentsApi.md#getenvironments) | **GET** /environments | List launch environments (no auth required)
*GpuTypesApi* | [**listGpuTypes**](docs/GpuTypesApi.md#listgputypes) | **GET** /gpu-types | List available GPU types (no auth required)
*InstancesApi* | [**createInstance**](docs/InstancesApi.md#createinstanceoperation) | **POST** /instances | Create an instance (async — returns 202 + Operation-Id header)
*InstancesApi* | [**deleteInstance**](docs/InstancesApi.md#deleteinstance) | **DELETE** /instances/{id} | Terminate an instance (idempotent)
*InstancesApi* | [**getInstance**](docs/InstancesApi.md#getinstance) | **GET** /instances/{id} | Get an instance
*InstancesApi* | [**listInstances**](docs/InstancesApi.md#listinstances) | **GET** /instances | List instances
*InstancesApi* | [**updateInstance**](docs/InstancesApi.md#updateinstanceoperation) | **PATCH** /instances/{id} | Update an instance (rename only in v1)
*MetaApi* | [**getHealth**](docs/MetaApi.md#gethealth) | **GET** /health | Liveness probe
*MetaApi* | [**getOpenApiSpec**](docs/MetaApi.md#getopenapispec) | **GET** /openapi.json | OpenAPI 3.1 specification
*OperationsApi* | [**getOperation**](docs/OperationsApi.md#getoperation) | **GET** /operations/{id} | Get an async operation status
*PricingApi* | [**listPricing**](docs/PricingApi.md#listpricing) | **GET** /pricing | List pricing per GPU type + region (no auth required)
*RegistryCredentialsApi* | [**createRegistryCredential**](docs/RegistryCredentialsApi.md#createregistrycredentialoperation) | **POST** /registry-credentials | Store a registry credential
*RegistryCredentialsApi* | [**deleteRegistryCredential**](docs/RegistryCredentialsApi.md#deleteregistrycredential) | **DELETE** /registry-credentials/{id} | Delete a registry credential
*RegistryCredentialsApi* | [**listRegistryCredentials**](docs/RegistryCredentialsApi.md#listregistrycredentials) | **GET** /registry-credentials | List registry credentials
*SshKeysApi* | [**createSshKey**](docs/SshKeysApi.md#createsshkeyoperation) | **POST** /ssh-keys | Create an SSH key
*SshKeysApi* | [**deleteSshKey**](docs/SshKeysApi.md#deletesshkey) | **DELETE** /ssh-keys/{id} | Delete an SSH key (idempotent; 404 when already gone)
*SshKeysApi* | [**getSshKey**](docs/SshKeysApi.md#getsshkey) | **GET** /ssh-keys/{id} | Get an SSH key
*SshKeysApi* | [**listSshKeys**](docs/SshKeysApi.md#listsshkeys) | **GET** /ssh-keys | List SSH keys


### Models

- [CreateInstanceRequest](docs/CreateInstanceRequest.md)
- [CreateInstanceRequestPortsInner](docs/CreateInstanceRequestPortsInner.md)
- [CreateRegistryCredentialRequest](docs/CreateRegistryCredentialRequest.md)
- [CreateSshKeyRequest](docs/CreateSshKeyRequest.md)
- [Environments](docs/Environments.md)
- [FrameworkVersion](docs/FrameworkVersion.md)
- [GPUType](docs/GPUType.md)
- [GPUTypePage](docs/GPUTypePage.md)
- [GetHealth200Response](docs/GetHealth200Response.md)
- [IncludeItem](docs/IncludeItem.md)
- [Instance](docs/Instance.md)
- [InstanceConnection](docs/InstanceConnection.md)
- [InstancePage](docs/InstancePage.md)
- [ListRegistryCredentials200Response](docs/ListRegistryCredentials200Response.md)
- [Operation](docs/Operation.md)
- [OperationError](docs/OperationError.md)
- [OperationResult](docs/OperationResult.md)
- [Pricing](docs/Pricing.md)
- [PricingPage](docs/PricingPage.md)
- [Problem](docs/Problem.md)
- [RegistryCredential](docs/RegistryCredential.md)
- [SSHKey](docs/SSHKey.md)
- [SSHKeyPage](docs/SSHKeyPage.md)
- [UpdateInstanceRequest](docs/UpdateInstanceRequest.md)
- [VMImage](docs/VMImage.md)

### Authorization


Authentication schemes defined for the API:
<a id="bearerAuth"></a>
#### bearerAuth


- **Type**: HTTP Bearer Token authentication

## About

This TypeScript SDK client supports the [Fetch API](https://fetch.spec.whatwg.org/)
and is automatically generated by the
[OpenAPI Generator](https://openapi-generator.tech) project:

- API version: `1.0.0`
- Package version: `0.1.0`
- Generator version: `7.24.0`
- Build package: `org.openapitools.codegen.languages.TypeScriptFetchClientCodegen`

The generated npm module supports the following:

- Environments
  * Node.js
  * Webpack
  * Browserify
- Language levels
  * ES5 - you must have a Promises/A+ library installed
  * ES6
- Module systems
  * CommonJS
  * ES6 module system


## Development

### Building

To build the TypeScript source code, you need to have Node.js and npm installed.
After cloning the repository, navigate to the project directory and run:

```bash
npm install
npm run build
```

## License

Apache-2.0
