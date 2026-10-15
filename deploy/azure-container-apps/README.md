# kista on Azure Container Apps

A Bicep recipe: kista with a user-assigned managed identity as its only credential, a Key Vault
HSM key, Blob Storage without shared keys, PostgreSQL Flexible Server or Azure SQL by Entra
authentication, and one Container Apps job per admin command.

```sh
az group create -n kista -l northeurope
cp examples/trial.bicepparam my.bicepparam
az deployment group create -g kista -f main.bicep -p my.bicepparam
```

The documentation: https://hugr-lab.github.io/duckdb-extension-repository/docs/deployment/azure
