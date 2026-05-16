import globals from 'globals';
import tsParser from '@typescript-eslint/parser';
import tsPlugin from '@typescript-eslint/eslint-plugin';
import * as svelte from 'eslint-plugin-svelte';

const jsTsFiles = ['**/*.{js,ts}'];
const svelteFiles = ['**/*.svelte'];

const tsConfigs = [{
  ...tsPlugin.configs['flat/base'],
  files: jsTsFiles,
}];

export default [
  {
    ignores: ['dist/**', 'node_modules/**'],
  },
  ...tsConfigs,
  ...svelte.configs['flat/base'],
  {
    files: jsTsFiles,
    languageOptions: {
      ecmaVersion: 'latest',
      globals: {
        ...globals.browser,
        ...globals.node,
      },
      parser: tsParser,
      sourceType: 'module',
    },
  },
  {
    files: svelteFiles,
    languageOptions: {
      globals: {
        ...globals.browser,
        ...globals.node,
      },
      parserOptions: {
        ecmaVersion: 'latest',
        parser: tsParser,
        sourceType: 'module',
      },
    },
  },
];
