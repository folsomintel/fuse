// doctor against a mock orchestrator: the path it hits and how the report
// decodes, including a status this sdk has not learned yet.

use fuse::{Client, DoctorStatus, Error};
use serde_json::json;
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

#[tokio::test]
async fn doctor_reads_the_report() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-1/doctor"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "status": "fail",
            "vm_id": "vm-1",
            "version": "v1.4.0",
            "checks": [
                {"name": "display", "status": "ok", "message": ":1 1280x800"},
                {"name": "vnc", "status": "fail", "message": "vnc server not reachable"},
                {"name": "healthcheck", "status": "skip"},
                {"name": "gpu", "status": "degraded"},
            ],
        })))
        .mount(&server)
        .await;

    let client = Client::new(server.uri(), "secret").unwrap();
    let report = client.environments().doctor("vm-1").await.unwrap();
    assert_eq!(report.status, DoctorStatus::Fail);
    assert_eq!(report.version, "v1.4.0");
    assert_eq!(report.vm_id.as_deref(), Some("vm-1"));
    assert_eq!(report.checks.len(), 4);
    assert_eq!(report.checks[0].message.as_deref(), Some(":1 1280x800"));
    assert_eq!(report.checks[2].status, DoctorStatus::Skip);
    assert_eq!(report.checks[2].message, None);
    assert_eq!(
        report.checks[3].status,
        DoctorStatus::Other("degraded".to_owned())
    );
}

#[tokio::test]
async fn doctor_rejects_an_empty_vm_id() {
    let client = Client::new("http://127.0.0.1:1", "secret").unwrap();
    assert!(matches!(
        client.environments().doctor("").await,
        Err(Error::InvalidRequest(_))
    ));
}
