import { createTheme, rem } from '@mantine/core';

// A dark-first console theme. The brand accent is a Kubernetes-adjacent blue; components get a
// slightly larger default radius for the soft, modern card look.
export const theme = createTheme({
  primaryColor: 'brand',
  defaultRadius: 'md',
  fontFamily:
    '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif',
  fontFamilyMonospace: 'ui-monospace, SFMono-Regular, Menlo, Monaco, "Liberation Mono", monospace',
  headings: {
    fontWeight: '650',
    sizes: {
      h1: { fontSize: rem(28) },
      h2: { fontSize: rem(22) },
      h3: { fontSize: rem(18) },
    },
  },
  colors: {
    brand: [
      '#e7f0ff',
      '#cfe0ff',
      '#9dbffb',
      '#679cf6',
      '#3c7ef2',
      '#216bf0',
      '#0f61f0',
      '#0051d6',
      '#0048c0',
      '#003ca8',
    ],
  },
  components: {
    Card: { defaultProps: { withBorder: true, shadow: 'sm' } },
    Paper: { defaultProps: { withBorder: true } },
  },
});
