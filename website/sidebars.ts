import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  docsSidebar: [
    'index',
    'getting-started',
    'concepts',
    'duckdb',
    'configuration',
    {
      type: 'category',
      label: 'Deployment',
      link: {type: 'doc', id: 'deployment/index'},
      items: ['deployment/kubernetes'],
    },
    'state',
    'signing-keys',
    'storage',
    'publishing',
    'upstreams',
    'access',
    'attachments',
    'api',
    'console',
    {
      type: 'category',
      label: 'Administration',
      link: {type: 'doc', id: 'administration/index'},
      items: [
        'administration/commands',
        'administration/tenants-and-channels',
        'administration/key-rotation',
        'administration/upgrading',
        'administration/backup-restore',
      ],
    },
    'observability',
    'security',
    'development',
  ],
};

export default sidebars;
