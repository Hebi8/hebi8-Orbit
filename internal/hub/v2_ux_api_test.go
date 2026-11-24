package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

func TestV2UXEndpoints_WorkspaceArtifactsAndDeleteAnnotation(t *testing.T) {
	repoPath := t.TempDir()
	_, st, ts := setupPhase1Hub(t, repoPath)
	ctx := context.Background()

	// 1. Bootstrap includes default WorkspaceName ("Home LAN")
	resp, err := http.Get(ts.URL + "/api/bootstrap")
	if err != nil {
		t.Fatalf("GET /api/bootstrap: %v", err)
	}
	defer resp.Body.Close()
	var boot protocol.BootstrapResponse
	if err := json.NewDecoder(resp.Body).Decode(&boot); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if boot.WorkspaceName != "Home LAN" {
		t.Fatalf("expected default WorkspaceName 'Home LAN', got %q", boot.WorkspaceName)
	}

	// 2. Create task with runtime config and verify TaskTimeline.RuntimeConfig
	createBody, _ := json.Marshal(protocol.CreateTaskRequest{
		Goal: "Write arch spec and code",
		Config: map[string]string{
			"model":         "opus-4.6",
			"thought_level": "high",
			"mode":          "auto",
		},
	})
	cReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/projects/demo-app/tasks", bytes.NewReader(createBody))
	cReq.Header.Set("Content-Type", "application/json")
	cReq.Header.Set("X-Orbit-Request", "1")
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatalf("POST create task: %v", err)
	}
	defer cResp.Body.Close()
	var created protocol.CreateTaskResponse
	if err := json.NewDecoder(cResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create task: %v", err)
	}
	if created.Attempt == nil {
		t.Fatalf("expected initial attempt")
	}

	tlResp, err := http.Get(ts.URL + "/api/tasks/" + created.Task.TaskID + "/timeline")
	if err != nil {
		t.Fatalf("GET timeline: %v", err)
	}
	defer tlResp.Body.Close()
	var tl protocol.TaskTimelineResponse
	if err := json.NewDecoder(tlResp.Body).Decode(&tl); err != nil {
		t.Fatalf("decode timeline: %v", err)
	}
	if tl.RuntimeConfig["model"] != "opus-4.6" || tl.RuntimeConfig["thought_level"] != "high" {
		t.Fatalf("expected RuntimeConfig persisted on timeline, got %+v", tl.RuntimeConfig)
	}

	// 3. Publish delivery with mixed blobs (image should be excluded from global Artifacts; md & pptx included) + Ryugi binding
	_ = st.PutBlobRecord(ctx, "1111111111111111111111111111111111111111111111111111111111111111", 1024, "/tmp/b1")
	_ = st.PutBlobRecord(ctx, "2222222222222222222222222222222222222222222222222222222222222222", 2048, "/tmp/b2")
	_ = st.PutBlobRecord(ctx, "3333333333333333333333333333333333333333333333333333333333333333", 4096, "/tmp/b3")
	dlvRes, err := st.CreateManualImportDelivery(ctx, store.ManualImportParams{
		ProjectID:       "demo-app",
		TaskID:          created.Task.TaskID,
		SourceMachineID: "hub-node",
		RepoIdentity:    "github.com/example-org/demo-app",
		BaseSHA:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadSHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		GitRef:          "refs/orbit/deliveries/test",
		Kind:            protocol.DeliveryMixed,
		Summary:         "spec + slides + diagram",
		Manifest: protocol.DeliveryManifest{
			FilesChanged: 2,
			Insertions:   42,
			Deletions:    7,
			Blobs: []protocol.BlobManifestItem{
				{Path: "docs/diagram.png", SHA256: "1111111111111111111111111111111111111111111111111111111111111111", Size: 1024},
				{Path: "docs/arch.md", SHA256: "2222222222222222222222222222222222222222222222222222222222222222", Size: 2048},
				{Path: "slides/review.pptx", SHA256: "3333333333333333333333333333333333333333333333333333333333333333", Size: 4096},
			},
			RyugiArtifacts: []string{"artboard/orbit-ux"},
		},
		ActorID: "test",
	})
	if err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	if _, _, err := st.SaveRyugiBindings(ctx, dlvRes.Delivery.DeliveryID, "/tmp/frozen", protocol.RyugiSyncReady, []protocol.RyugiBindingView{
		{
			BindingID:        "rb_1",
			DeliveryID:       dlvRes.Delivery.DeliveryID,
			ProjectID:        "demo-app",
			TaskID:           created.Task.TaskID,
			FrozenProjectDir: "/tmp/frozen",
			ArtifactLocator:  "artboard/orbit-ux",
			DerivedArtifactID: "abc",
			OpenURL:          "https://orbit.example.ts.net:8443/artboard/abc",
			Active:           true,
		},
	}); err != nil {
		t.Fatalf("save ryugi binding: %v", err)
	}

	artResp, err := http.Get(ts.URL + "/api/artifacts")
	if err != nil {
		t.Fatalf("GET /api/artifacts: %v", err)
	}
	defer artResp.Body.Close()
	var arts protocol.ListArtifactsResponse
	if err := json.NewDecoder(artResp.Body).Decode(&arts); err != nil {
		t.Fatalf("decode artifacts: %v", err)
	}
	if len(arts.Artifacts) != 3 {
		t.Fatalf("expected 3 reusable artifacts (ryugi + md + pptx, excluding png), got %d: %+v", len(arts.Artifacts), arts.Artifacts)
	}

	// 4. Create open annotation with line range + quote, then delete it via DELETE /api/annotations/{id}
	annBody, _ := json.Marshal(protocol.CreateAnnotationRequest{
		Source: protocol.AnnotationSourceDiffDrawer,
		Anchor: protocol.AnnotationAnchor{
			Path:      "main.go",
			Side:      "new",
			StartLine: 10,
			EndLine:   14,
			Quote:     "return nil",
		},
		Body: "handle error here",
	})
	aReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/deliveries/"+dlvRes.Delivery.DeliveryID+"/annotations", bytes.NewReader(annBody))
	aReq.Header.Set("Content-Type", "application/json")
	aReq.Header.Set("X-Orbit-Request", "1")
	aResp, err := http.DefaultClient.Do(aReq)
	if err != nil {
		t.Fatalf("POST annotation: %v", err)
	}
	defer aResp.Body.Close()
	var createdAnn protocol.CreateAnnotationResponse
	if err := json.NewDecoder(aResp.Body).Decode(&createdAnn); err != nil {
		t.Fatalf("decode annotation: %v", err)
	}

	dReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/annotations/"+createdAnn.Annotation.AnnotationID, nil)
	dReq.Header.Set("X-Orbit-Request", "1")
	dResp, err := http.DefaultClient.Do(dReq)
	if err != nil {
		t.Fatalf("DELETE annotation: %v", err)
	}
	defer dResp.Body.Close()
	if dResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on delete open annotation, got %d", dResp.StatusCode)
	}

	// Deleting again returns 404
	dReq2, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/annotations/"+createdAnn.Annotation.AnnotationID, nil)
	dReq2.Header.Set("X-Orbit-Request", "1")
	dResp2, err := http.DefaultClient.Do(dReq2)
	if err != nil {
		t.Fatalf("DELETE annotation 2: %v", err)
	}
	defer dResp2.Body.Close()
	if dResp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on deleting missing annotation, got %d", dResp2.StatusCode)
	}
}
