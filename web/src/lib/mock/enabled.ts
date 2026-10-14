// Mock switch. A public variable injected at build time (only a NEXT_PUBLIC_ prefix is readable in the browser).
// Set NEXT_PUBLIC_MOCK=1 on Vercel and the whole site uses mock data with no backend.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
