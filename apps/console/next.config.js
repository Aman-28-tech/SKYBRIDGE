/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  output: "standalone",
  env: {
    NEXT_PUBLIC_CONTROL_PLANE_URL:
      process.env.NEXT_PUBLIC_CONTROL_PLANE_URL || "http://localhost:18080",
  },
};

module.exports = nextConfig;
