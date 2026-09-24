# Upstream price and balance monitor

This fork adds an opt-in monitor for NewAPI and sub2api channels. It does not
run against an upstream until both the global switch and the channel switch
are enabled. Automatic repricing and automatic balance-based disablement have
separate switches, both off by default.

## Configuration

1. Set `UPSTREAM_MONITOR_ENCRYPTION_KEY` to a random value of at least 32
   characters in the deployment environment. Use the same persistent value on
   every NewAPI node. Back it up securely: losing it makes stored monitor
   credentials unreadable. Do not put it in Git.
2. After a database backup, deploy the fork and let its normal migration add
   the three `upstream_monitor*` tables. Keep automatic actions off.
3. Under **System Settings > Operations > Upstream Monitor**, configure the
   scan intervals, spike threshold and optional DingTalk robot webhook. The
   webhook is write-only in the UI and encrypted in the database.
4. In each channel row, open the monitor control beside **Test Connection**.
   Enter the upstream's public HTTPS root URL (not `/v1`), upstream group and
   credentials. NewAPI needs a user ID plus dashboard access token (not a
   relay `sk-` key); sub2api needs an email and password. The monitor identity
   must be billed at the same rates as the channel's relay key.
5. Enable monitoring and use **Run now**. Review every model's normalized
   price, unsupported entries, balance and error state. Only after complete
   results match the upstream should automatic repricing or automatic
   balance disablement be enabled.

## Safety rules

- A group is repriced only when all enabled channels in that group have enabled
  monitors and a fresh, complete scan. Missing model mappings, unsupported
  billing modes and special user/group overrides pause that group's changes.
- The required group multiplier covers the highest component cost across all
  channel/model combinations. An increase below the spike threshold is applied
  on one full scan; a decrease requires two consecutive full scans. A spike
  pauses repricing and alerts, but **does not stop traffic**.
- Image/audio-specific charges and tiered pricing are marked for manual review
  rather than converted to text-token rates.
- Balance must be zero on two consecutive scans, and the account must have no
  active subscription, before a single-key channel may be auto-disabled.
  Multi-key channels never auto-disable. Recovery requires positive balance,
  ownership of the previous disable reason, and a successful `/v1/models`
  probe. With auto pricing enabled, a fresh price scan must also show that
  the current group multiplier covers the recovering channel's costs. Any
  failed or unsupported subscription check prevents auto-disable.
- Only public HTTPS targets on port 443 are accepted. Redirects are disabled,
  DNS results are checked again immediately before dialing, and credentials
  and webhook URLs are never returned by the management API.

No production upgrade or key rotation is performed by this feature. Validate
the fork against a staging database and a test upstream before rollout.
