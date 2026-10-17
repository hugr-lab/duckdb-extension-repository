// Azure Database for PostgreSQL Flexible Server in a delegated subnet, Entra authentication only:
// kista's identity is the server's Entra administrator (a member of azure_pg_admin on a server of
// kista's own; the docs give the narrower alternative).

param name string
param location string
param tags object
param subnetId string
param vnetId string
param sku string
param tier string
param version string
param adminPrincipalId string
param adminName string

resource zone 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: '${name}.private.postgres.database.azure.com'
  location: 'global'
  tags: tags
}

resource link 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2024-06-01' = {
  parent: zone
  name: 'vnet'
  location: 'global'
  properties: {
    virtualNetwork: { id: vnetId }
    registrationEnabled: false
  }
}

resource server 'Microsoft.DBforPostgreSQL/flexibleServers@2025-08-01' = {
  name: name
  location: location
  tags: tags
  sku: { name: sku, tier: tier }
  properties: {
    version: version
    storage: { storageSizeGB: 32, autoGrow: 'Enabled' }
    backup: { backupRetentionDays: 7, geoRedundantBackup: 'Disabled' }
    highAvailability: { mode: 'Disabled' }
    authConfig: { activeDirectoryAuth: 'Enabled', passwordAuth: 'Disabled', tenantId: subscription().tenantId }
    network: {
      delegatedSubnetResourceId: subnetId
      privateDnsZoneArmResourceId: zone.id
      publicNetworkAccess: 'Disabled'
    }
  }
  dependsOn: [link]
}

resource admin 'Microsoft.DBforPostgreSQL/flexibleServers/administrators@2025-08-01' = {
  parent: server
  name: adminPrincipalId
  properties: {
    principalType: 'ServicePrincipal'
    principalName: adminName
    tenantId: subscription().tenantId
  }
}

resource db 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2025-08-01' = {
  parent: server
  name: 'kista'
  properties: { charset: 'UTF8', collation: 'en_US.utf8' }
  dependsOn: [admin]
}

output fqdn string = server.properties.fullyQualifiedDomainName
