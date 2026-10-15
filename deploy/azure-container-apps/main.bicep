// kista on Azure Container Apps (spec 0020 phase 3): a user-assigned managed identity the only
// credential (no key, no password), signing with a Key Vault HSM key through a custom role that
// may only read and sign, bodies in Blob Storage without shared keys, the store on Azure Database
// for PostgreSQL (or Azure SQL) by Entra authentication over verify-full, behind Container Apps'
// ingress serving http and https without a redirect. One manual job per admin command.

targetScope = 'resourceGroup'

@description('Names every resource. Lower-case letters, digits and hyphens.')
@minLength(2)
@maxLength(10)
param prefix string = 'kista'

param location string = resourceGroup().location

@description('kista\'s image: ghcr.io/hugr-lab/kista:vX.Y.Z or …@sha256:… (a moving tag only pinned by digest).')
param image string

@description('kista\'s public host name, bound to the app as a custom domain by the operator; empty: the app\'s own name under azurecontainerapps.io.')
param domain string = ''

@description('auth.server_issuers: the identity provider of the people who administer kista.')
param serverIssuers array

@description('auth.server_audiences; empty: the public URL. Entra issues api:// audiences or the app\'s client id, not an https one.')
param serverAudiences array = []

@description('auth.server_admins.')
param serverAdmins array

@description('The store: PostgreSQL Flexible Server or Azure SQL, Entra authentication only.')
@allowed(['postgres', 'sqlserver'])
param database string = 'postgres'

@description('The CA bundle the PostgreSQL server is verified against (store.ca_pem).')
param postgresCaPem string = loadTextContent('ca/azure-postgres.pem')

param postgresSku string = 'Standard_B1ms'
param postgresTier string = 'Burstable'
param postgresVersion string = '17'
param sqlSku string = 'GP_S_Gen5_1'

@description('vCPUs per replica; above 1, a replica has 8 GiB of ephemeral storage, the spool included.')
@allowed(['0.5', '1.0', '2.0', '4.0'])
param cpu string = '2.0'

@minValue(1)
param replicas int = 2

@description('blob.max_body in MiB.')
@minValue(1)
param maxBodyMib int = 1024

@minValue(1)
param maxIngests int = 2

@description('Key Vault purge protection: a deleted vault and its keys stay recoverable for the retention period. Off only for a trial.')
param purgeProtection bool = true

@minValue(7)
@maxValue(90)
param softDeleteDays int = 90

param tags object = {}

var name = prefix
var unique = uniqueString(resourceGroup().id, prefix)
var tenantId = subscription().tenantId
var allTags = union({ app: 'kista', prefix: prefix }, tags)
var infraCidr = '10.50.0.0/23'

// the spool, (2 × max_ingests + 1) × max_body, on the replica's ephemeral storage with 1 GiB to spare
var ephemeralMib = json(cpu) > 1 ? 8192 : int(json(cpu) * 4096)
var spoolMib = (2 * maxIngests + 1) * maxBodyMib
var spoolCheck = spoolMib <= ephemeralMib - 1024 ? 'ok' : int('the spool, (2 x maxIngests + 1) x maxBodyMib, exceeds the replica\'s ephemeral storage less 1 GiB')

// ---- the identity: no key; every service admits it by role

resource identity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${name}-identity'
  location: location
  tags: allTags
}

// ---- the network: the environment's subnet is the range kista trusts for X-Forwarded-For

resource vnet 'Microsoft.Network/virtualNetworks@2024-05-01' = {
  name: '${name}-vnet'
  location: location
  tags: allTags
  properties: {
    addressSpace: { addressPrefixes: ['10.50.0.0/16'] }
    subnets: [
      {
        name: 'apps'
        properties: {
          addressPrefix: infraCidr
          delegations: [{ name: 'apps', properties: { serviceName: 'Microsoft.App/environments' } }]
        }
      }
      {
        name: 'postgres'
        properties: {
          addressPrefix: '10.50.2.0/27'
          delegations: [{ name: 'postgres', properties: { serviceName: 'Microsoft.DBforPostgreSQL/flexibleServers' } }]
        }
      }
      {
        name: 'endpoints'
        properties: {
          addressPrefix: '10.50.3.0/27'
        }
      }
    ]
  }
}

// ---- Key Vault Premium: the channel key in an HSM; kista may read and sign, nothing else

resource vault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: take('${name}-${unique}', 24)
  location: location
  tags: allTags
  properties: {
    tenantId: tenantId
    sku: { family: 'A', name: 'premium' }
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: softDeleteDays
    enablePurgeProtection: purgeProtection ? true : null
    publicNetworkAccess: 'Enabled'
  }
}

resource channelKey 'Microsoft.KeyVault/vaults/keys@2023-07-01' = {
  parent: vault
  name: '${name}-channel'
  tags: { 'kista-purpose': 'channel-signing' }
  properties: {
    kty: 'RSA-HSM'
    keySize: 2048
    // azurekv refuses a key that may do anything else
    keyOps: ['sign', 'verify']
    attributes: { enabled: true, exportable: false }
  }
}

// creating a role definition needs Owner or User Access Administrator on the group
resource signerRole 'Microsoft.Authorization/roleDefinitions@2022-04-01' = {
  name: guid(resourceGroup().id, prefix, 'kista-signer')
  properties: {
    roleName: 'kista key signer (${resourceGroup().name}/${prefix})'
    description: 'Read and sign with Key Vault keys: kista\'s channel signing.'
    type: 'CustomRole'
    assignableScopes: [resourceGroup().id]
    permissions: [
      {
        actions: []
        notActions: []
        dataActions: ['Microsoft.KeyVault/vaults/keys/read', 'Microsoft.KeyVault/vaults/keys/sign/action']
        notDataActions: []
      }
    ]
  }
}

resource signerAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: vault
  name: guid(vault.id, identity.id, signerRole.id)
  properties: {
    roleDefinitionId: signerRole.id
    principalId: identity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

// ---- Blob Storage: no shared keys, no public access; the identity on its container only

resource storage 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: take('${replace(name, '-', '')}${unique}', 24)
  location: location
  tags: allTags
  kind: 'StorageV2'
  sku: { name: 'Standard_LRS' }
  properties: {
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    defaultToOAuthAuthentication: true
  }
}

resource blobService 'Microsoft.Storage/storageAccounts/blobServices@2023-05-01' = {
  parent: storage
  name: 'default'
  properties: {
    deleteRetentionPolicy: { enabled: true, days: 7 }
  }
}

resource container 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-05-01' = {
  parent: blobService
  name: 'kista'
  properties: { publicAccess: 'None' }
}

var blobContributor = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'ba92f5b4-2d11-453d-a403-e96b0029c9fe')

resource blobAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: container
  name: guid(container.id, identity.id, blobContributor)
  properties: {
    roleDefinitionId: blobContributor
    principalId: identity.properties.principalId
    principalType: 'ServicePrincipal'
  }
}

// ---- the store

module postgres 'modules/postgres.bicep' = if (database == 'postgres') {
  name: '${name}-postgres'
  params: {
    name: '${name}-${unique}'
    location: location
    tags: allTags
    subnetId: '${vnet.id}/subnets/postgres'
    vnetId: vnet.id
    sku: postgresSku
    tier: postgresTier
    version: postgresVersion
    adminPrincipalId: identity.properties.principalId
    adminName: identity.name
  }
}

module sql 'modules/sql.bicep' = if (database == 'sqlserver') {
  name: '${name}-sql'
  params: {
    name: '${name}-${unique}'
    location: location
    tags: allTags
    subnetId: '${vnet.id}/subnets/endpoints'
    vnetId: vnet.id
    sku: sqlSku
    adminClientId: identity.properties.clientId
    adminName: identity.name
  }
}

var store = database == 'postgres'
  ? {
      kind: 'postgres'
      dsn: 'postgres://${identity.name}@${postgres!.outputs.fqdn}:5432/kista?sslmode=verify-full'
      login: { kind: 'entra' }
      ca_pem: postgresCaPem
    }
  : {
      kind: 'sqlserver'
      dsn: 'sqlserver://${sql!.outputs.fqdn}:1433?database=kista&encrypt=strict'
      login: { kind: 'entra' }
    }

// ---- Container Apps

resource logs 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: '${name}-logs'
  location: location
  tags: allTags
  properties: {
    sku: { name: 'PerGB2018' }
    retentionInDays: 30
  }
}

resource env 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: '${name}-env'
  location: location
  tags: allTags
  properties: {
    vnetConfiguration: {
      infrastructureSubnetId: '${vnet.id}/subnets/apps'
      internal: false
    }
    workloadProfiles: [{ name: 'Consumption', workloadProfileType: 'Consumption' }]
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logs.properties.customerId
        sharedKey: logs.listKeys().primarySharedKey
      }
    }
  }
}

var host = empty(domain) ? '${name}.${env.properties.defaultDomain}' : domain
var managed = { kind: 'managed', client_id: identity.properties.clientId }

// kista's configuration: JSON is YAML, read with -config env:KISTA_CONFIG
var config = {
  azure: { identity: managed }
  store: store
  serve: {
    public_url: 'https://${host}'
    // the environment's Envoy reaches the replicas from its subnet
    trusted_proxies: [infraCidr]
    listeners: [
      { addr: ':8443', scheme: 'https', behind_proxy: true }
      { addr: ':8081', scheme: 'health' }
    ]
  }
  signers: {
    sources: [
      {
        name: 'azure'
        kind: 'azurekv'
        allow: ['*']
        azurekv: { vault: vault.name }
        identity: managed
      }
    ]
  }
  blob: {
    spool_dir: '/var/lib/kista/tmp/spool'
    max_body: '${maxBodyMib}MiB'
    max_ingests: maxIngests
    domains: [
      {
        name: 'default'
        kind: 'azureblob'
        azureblob: { account: storage.name, container: container.name, identity: managed }
      }
    ]
  }
  auth: union(
    { server_issuers: serverIssuers, server_admins: serverAdmins },
    empty(serverAudiences) ? {} : { server_audiences: serverAudiences }
  )
}

var memory = '${int(json(cpu) * 2)}Gi'
var container_ = {
  name: 'kista'
  image: image
  env: [{ name: 'KISTA_CONFIG', value: string(config) }]
  resources: { cpu: json(cpu), memory: json(cpu) < 1 ? '1Gi' : memory }
  volumeMounts: [{ volumeName: 'tmp', mountPath: '/var/lib/kista/tmp' }]
}
var volumes = [{ name: 'tmp', storageType: 'EmptyDir' }]

resource app 'Microsoft.App/containerApps@2024-03-01' = {
  name: name
  location: location
  tags: union(allTags, { 'kista-spool': spoolCheck })
  identity: { type: 'UserAssigned', userAssignedIdentities: { '${identity.id}': {} } }
  properties: {
    environmentId: env.id
    workloadProfileName: 'Consumption'
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: {
        external: true
        targetPort: 8443
        transport: 'http'
        // http is served without a redirect: DuckDB's bootstrap follows none
        allowInsecure: true
        traffic: [{ latestRevision: true, weight: 100 }]
      }
    }
    template: {
      containers: [
        union(container_, {
          args: ['serve', '-config', 'env:KISTA_CONFIG']
          probes: [
            { type: 'Startup', httpGet: { port: 8081, path: '/healthz' }, periodSeconds: 10, failureThreshold: 30 }
            { type: 'Liveness', httpGet: { port: 8081, path: '/healthz' }, periodSeconds: 10, failureThreshold: 6 }
            { type: 'Readiness', httpGet: { port: 8081, path: '/readyz' }, periodSeconds: 5, failureThreshold: 2 }
          ]
        })
      ]
      volumes: volumes
      terminationGracePeriodSeconds: 120
      scale: { minReplicas: replicas, maxReplicas: replicas }
    }
  }
  dependsOn: [signerAssignment, blobAssignment]
}

// ---- admin commands: one manual job each (a start with --args would replace the whole template)

var commands = [
  { name: 'migrate', args: ['migrate'] }
  { name: 'blob-check', args: ['blob', 'check'] }
  { name: 'blob-verify', args: ['blob', 'verify'] }
  { name: 'blob-gc', args: ['blob', 'gc', '-apply'] }
]

resource jobs 'Microsoft.App/jobs@2024-03-01' = [
  for c in commands: {
    name: '${name}-${c.name}'
    location: location
    tags: allTags
    identity: { type: 'UserAssigned', userAssignedIdentities: { '${identity.id}': {} } }
    properties: {
      environmentId: env.id
      workloadProfileName: 'Consumption'
      configuration: {
        triggerType: 'Manual'
        replicaTimeout: 3600
        replicaRetryLimit: 0
        manualTriggerConfig: { parallelism: 1, replicaCompletionCount: 1 }
      }
      template: {
        containers: [union(container_, { args: concat(['admin', '-config', 'env:KISTA_CONFIG'], c.args) })]
        volumes: volumes
      }
    }
    dependsOn: [signerAssignment, blobAssignment]
  }
]

output url string = 'https://${host}'
output fqdn string = app.properties.configuration.ingress.fqdn
output identityClientId string = identity.properties.clientId
output identityPrincipalId string = identity.properties.principalId
output channelSigner string = 'azure:${channelKey.name}/${last(split(channelKey.properties.keyUriWithVersion, '/'))}'
output vault string = vault.name
output storageAccount string = storage.name
output environment string = env.name
output jobs array = [for (c, i) in commands: jobs[i].name]
output databaseHost string = database == 'postgres' ? postgres!.outputs.fqdn : sql!.outputs.fqdn
