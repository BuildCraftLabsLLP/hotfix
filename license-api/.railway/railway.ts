import { defineRailway, postgres, project, service, volume } from "railway/iac";

export default defineRailway(() => {
  const Postgres = postgres("Postgres", { region: "us-east4-eqdc4a" });
  Postgres.networking = { privateNetworkEndpoint: "postgres" };
  const postgresVolume = volume("postgres-volume", { alerts: { usage: { "100": {}, "80": {}, "95": {} } }, allowOnlineResize: true, region: "us-east4-eqdc4a", sizeMB: 5000 });
  // The licensing API (license-api/: Dockerfile, src/server.js). Variables
  // (DATABASE_URL, Razorpay, signing key) are set in Railway, not here.
  const licenseApi = service("license-api", {
    replicas: { "us-east4-eqdc4a": 1 },
    domains: ["license.buildcraft.town"],
    healthcheck: "/healthz",
  });

  return project("hotfix-license", {
    variables: { managed: false },
    resources: [licenseApi, Postgres, postgresVolume],
  });
});
