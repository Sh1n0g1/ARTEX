// Mock toggle. A public variable injected at build time (only NEXT_PUBLIC_-prefixed
// vars are readable in the browser). Setting NEXT_PUBLIC_MOCK=1 on Vercel makes the
// whole site run on mock data with no backend needed.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
