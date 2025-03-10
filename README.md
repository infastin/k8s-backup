# k8s-backup

Very simple backup tool for Kubernetes that scales down a workload (Deployment/StatefulSet/ReplicaSet),
creates an archive of the specified directory, uploads it to S3 and scales the workload back up.

## Configuration

<table>
  <tr>
    <th>Environment variable</th>
    <th>Type</th>
    <th>Description</th>
  </tr>
  <tr>
    <td>RESOURCE_ID</td>
    <td>string</td>
    <td>Resource identifer in form of TYPE/NAME,<br>where TYPE is deployment(s), statefulset(s) or replicaset(s).</td>
  </tr>
  <tr>
    <td>RESOURCE_NAMESPACE</td>
    <td>string</td>
    <td>Namespace where workload resides.</td>
  </tr>
  <tr>
    <td>RESOURCE_WAIT</td>
    <td>boolean</td>
    <td>Wait for pods to terminate if true.</td>
  </tr>
  <tr>
    <td>BACKUP_DIRECTORY</td>
    <td>string</td>
    <td>Directory to backup.</td>
  </tr>
  <tr>
    <td>S3_ENDPOINT</td>
    <td>string</td>
    <td>S3 endpoint address (domain or ip address).</td>
  </tr>
  <tr>
    <td>S3_REGION</td>
    <td>string</td>
    <td>S3 region (can be empty).</td>
  </tr>
  <tr>
    <td>S3_ACCESS_KEY_ID</td>
    <td>string</td>
    <td>S3 access key id.</td>
  </tr>
  <tr>
    <td>S3_SECRET_ACCESS_KEY</td>
    <td>string</td>
    <td>S3 secret access key.</td>
  </tr>
  <tr>
    <td>S3_BUCKET</td>
    <td>string</td>
    <td>S3 bucket.</td>
  </tr>
  <tr>
    <td>S3_STORAGE_CLASS</td>
    <td>string</td>
    <td>S3 storage class (can be empty).</td>
  </tr>
  <tr>
    <td>S3_UNSECURE</td>
    <td>boolean</td>
    <td>Do not use SSL if true.</td>
  </tr>
  <tr>
    <td>S3_ARCHIVE_LIFETIME</td>
    <td>string</td>
    <td>Lifetime of the archive in the bucket (can be empty).<br>Supports <code>d</code> units.</td>
  </tr>
  <tr>
    <td>TELEGRAM_BOT_TOKEN</td>
    <td>string</td>
    <td>Telegram bot token from <code>@BotFather</code>.<br>If not empty, notifications will be sent by this bot.</td>
  </tr>
  <tr>
    <td>TELEGRAM_CHAT_ID</td>
    <td>integer</td>
    <td>Telegram chat id where notifications should be sent.</td>
  </tr>
</table>

## Kubernetes Role

This tool only does `get` and `patch` requests on `<TYPE>/scale`,
so a rule like this for scaling deployments will suffice:

```yaml
apiGroups:
  - apps
resources:
  - deployments/scale
verbs:
  - get
  - patch
```

However, if `RESOURCE_WAIT` is set,
this tool also does `get` requests on `apps/replicasets` and `pods`.
Therefore, you will also need these rules:

```yaml
- apiGroups:
    - apps
  resources:
    - replcasets
  verbs:
    - get
- apiGroups:
    - ""
  resources:
    - pods
  verbs:
    - get
```
