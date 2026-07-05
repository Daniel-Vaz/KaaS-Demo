# Security

The **Security** page shows a cluster's security posture, read from the **Trivy Operator**'s report
CRDs - Trivy scans the cluster from the inside and writes its findings back as custom resources, and
this page reads them. The operator ships in every default cluster. Pick a cluster at the top.

![The Security Overview: severity rollups, most-vulnerable images, and per-namespace risk](../assets/security-overview.png)

## Overview

A cluster-wide summary: per-kind severity rollups, the most vulnerable images, and per-namespace risk -
the fastest way to see where to look first.

## The report families

Four tabs, one per Trivy report type. Each is a searchable, severity-filterable table you can drill
into.

**Vulnerabilities** - image CVEs across the cluster's workloads.

![The vulnerability report list](../assets/security-vuln-list.png)
![A vulnerability finding in detail](../assets/security-vuln-details.png)

**Misconfigurations** - workload configuration audit findings (ConfigAuditReports).

![The misconfigurations list](../assets/security-misconfigurations-list.png)
![A misconfiguration finding in detail](../assets/security-misconfigurations-details.png)

**Exposed secrets** - secrets baked into container images (Trivy redacts the matched values).

![The exposed-secrets list](../assets/security-exposedsecrets-list.png)
![An exposed-secret finding in detail](../assets/security-exposedsecrets-details.png)

**RBAC assessment** - over-permissive Roles and ClusterRoles.

![The RBAC assessment list](../assets/security-rbacasses-list.png)
![An RBAC finding in detail](../assets/security-rbacasses-details.png)

## Notes

The page is **read-only** and view-scoped - anyone who can see the cluster can read its posture. It
needs no wiring beyond the add-on being installed: Trivy does the scanning inside the cluster, and this
page only reads what it wrote.
