import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

// Docs for kista, the trusted repository for DuckDB extensions. Published by this repository's Pages
// workflow (.github/workflows/pages.yml) to https://hugr-lab.github.io/duckdb-extension-repository/.
// The specs (specs/) are the design record; this site is how to run and use the server.

// The same docs are built twice: for GitHub Pages (default) and for hugr-lab.com on IONOS
// (DOCS_TARGET=ionos, uploaded by pages.yml). Neither requests anything from another host.
const ionos = process.env.DOCS_TARGET === 'ionos';
const site = ionos
  ? {url: 'https://hugr-lab.com', baseUrl: '/docs/duckdb-extension-repository/', home: 'https://hugr-lab.com/',
      impressum: 'https://hugr-lab.com/impressum/', datenschutz: 'https://hugr-lab.com/datenschutz/'}
  : {url: 'https://hugr-lab.github.io', baseUrl: '/duckdb-extension-repository/', home: 'https://hugr-lab.github.io/',
      impressum: 'https://hugr-lab.github.io/impressum/', datenschutz: 'https://hugr-lab.github.io/datenschutz/'};

// While the hugr-lab.com copy exists (DOCS_IONOS on), pages.yml sets DOCS_CANONICAL for the GitHub
// Pages build: its pages then name the hugr-lab.com copy as canonical (src/theme/SiteMetadata), and
// it has no sitemap of its own. Old github.io links keep working; search keeps hugr-lab.com.
const canonicalBase = ionos ? '' : process.env.DOCS_CANONICAL ?? '';

const config: Config = {
  title: 'kista',
  tagline: 'A trusted repository for DuckDB extensions: mirrored, checked, re-signed with your key.',
  favicon: 'img/favicon.svg',

  url: site.url,
  baseUrl: site.baseUrl,
  baseUrlIssueBanner: false,
  trailingSlash: true,
  customFields: {docsTarget: ionos ? 'ionos' : 'github', canonicalBase},

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
        sitemap: canonicalBase ? false : undefined,
        theme: {
          customCss: [
            require.resolve('@fontsource/quicksand/latin-400.css'),
            require.resolve('@fontsource/quicksand/latin-600.css'),
            './src/css/custom.css',
          ],
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
            {label: 'Main site', href: site.home},
            {label: 'tresor', href: 'https://hugr-lab.github.io/tresor/'},
            {label: 'GitHub', href: 'https://github.com/hugr-lab/duckdb-extension-repository'},
          ],
        },
        {
          title: 'Legal',
          items: [
            {label: 'Impressum', href: site.impressum},
            {label: 'Datenschutz', href: site.datenschutz},
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
