// Azure SQL, Entra authentication only, reached through a private endpoint: kista's identity is the
// server's Entra administrator (dbo in every database of a server of kista's own).
// The administrator of a service principal is named by its client id (sid).

param name string
param location string
param endpointLocation string
param tags object
param subnetId string
param vnetId string
param sku string
param adminClientId string
param adminName string

resource server 'Microsoft.Sql/servers@2025-01-01' = {
  name: name
  location: location
  tags: tags
  properties: {
    minimalTlsVersion: '1.2'
    publicNetworkAccess: 'Disabled'
    administrators: {
      administratorType: 'ActiveDirectory'
      azureADOnlyAuthentication: true
      login: adminName
      sid: adminClientId
      principalType: 'Application'
      tenantId: subscription().tenantId
    }
  }
}

resource db 'Microsoft.Sql/servers/databases@2025-01-01' = {
  parent: server
  name: 'kista'
  location: location
  tags: tags
  sku: { name: sku }
  // serverless: never paused, so kista's first connection after a quiet hour does not fail
  properties: { autoPauseDelay: -1, minCapacity: json('0.5') }
}

resource zone 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: 'privatelink${environment().suffixes.sqlServerHostname}'
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

resource endpoint 'Microsoft.Network/privateEndpoints@2025-05-01' = {
  name: '${name}-sql'
  location: endpointLocation
  tags: tags
  properties: {
    subnet: { id: subnetId }
    privateLinkServiceConnections: [
      { name: 'sql', properties: { privateLinkServiceId: server.id, groupIds: ['sqlServer'] } }
    ]
  }
}

resource zoneGroup 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2025-05-01' = {
  parent: endpoint
  name: 'default'
  properties: {
    privateDnsZoneConfigs: [{ name: 'sql', properties: { privateDnsZoneId: zone.id } }]
  }
}

output fqdn string = server.properties.fullyQualifiedDomainName
