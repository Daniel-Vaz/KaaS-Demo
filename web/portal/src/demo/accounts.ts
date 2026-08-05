// The demo's seeded accounts, shown on the login screen of a VITE_DEMO build.
//
// These mirror the constants in cmd/demo-wasm/seed.go, which is what actually creates them. There is
// nothing to protect: every visitor's control plane is a private WebAssembly module in their own tab
// with no storage behind it, and a demo nobody can sign in to is not a demo.
export interface DemoAccount {
  username: string;
  password: string;
  blurb: string;
}

export const DEMO_ACCOUNTS: DemoAccount[] = [
  { username: 'admin', password: 'kubeharbor', blurb: 'platform admin - sees every tenant, grants quota, manages groups' },
  { username: 'alice', password: 'kubeharbor', blurb: 'tenant with quota - owns the demo clusters' },
  { username: 'bob', password: 'kubeharbor', blurb: 'read-only group member - same clusters, no changes' },
];

/** True in the static demo build (see web/portal/src/demo/). */
export const IS_DEMO = Boolean(import.meta.env.VITE_DEMO);
