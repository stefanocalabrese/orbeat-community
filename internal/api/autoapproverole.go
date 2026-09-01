package api

// artifactAutoApproveRole is empty in Community: communityAutoApprove already
// approves every artifact on write, so a role that grants auto-approval would
// grant a power nobody lacks. See autoapproverole.ee.go for the Enterprise
// contract, and autoapprove.community.go for why the tag exists.
func artifactAutoApproveRole() string { return "" }
