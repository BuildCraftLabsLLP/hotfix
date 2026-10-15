import { bucket, defineRailway, postgres, project, service, volume } from "railway/iac";

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

  // Nightly encrypted database backups (backup/; Railway's own volume backups
  // need a Pro workspace). The bucket is in another region than the database.
  const backups = bucket("backups", { region: "sjc" });
  const pgBackup = service("pg-backup", {
    deploy: { cronSchedule: "30 2 * * *", restartPolicyType: "NEVER" }, // 02:30 UTC
  });

  return project("hotfix-license", {
    variables: { managed: false },
    resources: [licenseApi, Postgres, postgresVolume, backups, pgBackup],
  });
});
