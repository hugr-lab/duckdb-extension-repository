import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// Docs for kista, the trusted repository for DuckDB extensions. Published by this repository's Pages
// workflow (.github/workflows/pages.yml) to https://hugr-lab.github.io/duckdb-extension-repository/.
// The specs (specs/) are the design record; this site is how to run and use the server.

const config: Config = {
  title: 'kista',
  tagline: 'A trusted repository for DuckDB extensions: mirrored, checked, re-signed with your key.',
  favicon: 'img/favicon.svg',

  url: 'https://hugr-lab.github.io',
  baseUrl: '/duckdb-extension-repository/',
  trailingSlash: true,

  organizationName: 'hugr-lab',
  projectName: 'duckdb-extension-repository',

  // 'throw': docs-build.yml is the pull requests' gate for website/, and a gate that passes a broken
  // link gates nothing
  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          routeBasePath: '/',
          editUrl: 'https://github.com/hugr-lab/duckdb-extension-repository/tree/main/website/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themes: ['@docusaurus/theme-mermaid'],

  markdown: {
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  themeConfig: {
    metadata: [
      {name: 'keywords', content: 'DuckDB, extensions, extension repository, signing, mirror, Azure Key Vault, Vault, PostgreSQL, SQL Server'},
      {name: 'description', content: 'kista: a trusted repository for DuckDB extensions. Mirror core and community extensions, verify and re-sign them with your key, publish your own, serve public and private ones.'},
    ],
    navbar: {
      title: 'kista',
      logo: {
        alt: 'kista',
        src: 'img/logo.svg',
        href: '/',
      },
      items: [
        {type: 'docSidebar', sidebarId: 'docsSidebar', position: 'left', label: 'Docs'},
        {to: '/configuration/', label: 'Configuration', position: 'left'},
        {to: '/administration/commands/', label: 'Commands', position: 'left'},
        {href: 'https://github.com/hugr-lab/duckdb-extension-repository', label: 'GitHub', position: 'right'},
      ],
    },
    colorMode: {
      defaultMode: 'light',
      disableSwitch: true,
      respectPrefersColorScheme: false,
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {label: 'Getting started', to: '/getting-started/'},
            {label: 'Configuration', to: '/configuration/'},
            {label: 'Using it from DuckDB', to: '/duckdb/'},
            {label: 'Security', to: '/security/'},
          ],
        },
        {
          title: 'DuckDB',
          items: [
            {label: 'Extensions', href: 'https://duckdb.org/docs/stable/extensions/overview'},
            {label: 'Community extensions', href: 'https://duckdb.org/community_extensions/'},
          ],
        },
        {
          title: 'Hugr Lab',
          items: [
            {label: 'Main site', href: 'https://hugr-lab.github.io/'},
            {label: 'tresor', href: 'https://hugr-lab.github.io/tresor/'},
            {label: 'GitHub', href: 'https://github.com/hugr-lab/duckdb-extension-repository'},
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} Hugr Lab. Apache License 2.0.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['sql', 'bash', 'json', 'yaml'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
