# Google Tag Manager

## Prerequisites

- A Google Cloud project with the **Tag Manager API** enabled.
- A service account with access to the target GTM containers.

### Creating a service account

1. Open the [Google Cloud Console](https://console.cloud.google.com/).
2. Enable the **Tag Manager API** under **APIs & Services → Library**.
3. Go to **IAM & Admin → Service Accounts**.
4. Create a new service account.
5. Create a JSON key for the service account and download it.
6. Grant the service account access to the target container in Google Tag Manager (Container → Admin → User Management).

## Fields

| Field | Description |
|------|-------------|
| Service Account JSON Key | The full contents of the downloaded JSON key file. Stored in the Keychain. |

## After creating

Click **Test** to verify the connection. mux authenticates with the service account and lists the available containers.

## Notes

- The JSON key contains `client_email` and `private_key` — both are required.
- The service account needs at least read access to the container in GTM.
- Paste the entire JSON contents into the field, not the file path.
