# Disposable EKS qualification root

This isolated root creates only issue-103 qualification resources. It uses two
public worker subnets to avoid NAT hourly cost, bounded EKS API CIDRs, one
single-AZ On-Demand platform node, encrypted gp3 boot storage, IMDSv2, EKS Pod
Identity, and the EBS CSI add-on. It creates no EFS, load balancer, static AWS
credential, backup policy, or long-lived shared resource.

Always supply `owner`, unique `run_id`, RFC3339 `expires_at`, and bounded
`public_access_cidrs`. Save and review a binary plan before apply. Applying this
root requires Matt's separate explicit approval. Destroy from the same state,
then independently query the `kyber.io/run-id` tag in each native AWS service.
