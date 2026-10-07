// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.databind.ObjectMapper;
import sh.brewlet.maven.plugin.model.JvmConfig;

import java.io.IOException;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;

/** Fully assembled content shared by local layout writing and registry publication. */
public record ApplicationImage(List<RunnableImageBuilder.Blob> blobs,
                               List<RunnableImageBuilder.Blob> manifests,
                               RunnableImageBuilder.Blob root) {
    public ApplicationImage {
        blobs = List.copyOf(blobs);
        manifests = List.copyOf(manifests);
    }

    public static ApplicationImage runnable(RunnableImageBuilder.Result image) {
        return new ApplicationImage(image.blobs, image.manifests,
                new RunnableImageBuilder.Blob(image.indexDigest, image.indexBytes, MediaTypes.OCI_INDEX_MEDIA_TYPE));
    }

    public static ApplicationImage artifact(JvmConfig config, byte[] jar,
                                            List<ArtifactLayer> layers, Map<String, String> annotations)
            throws IOException {
        var json = new ObjectMapper().writerWithDefaultPrettyPrinter();
        List<RunnableImageBuilder.Blob> blobs = new ArrayList<>();
        var configBlob = blob(json.writeValueAsBytes(config), MediaTypes.CONFIG_MEDIA_TYPE);
        blobs.add(configBlob);
        var jarBlob = blob(jar, MediaTypes.JAR_LAYER_MEDIA_TYPE);
        blobs.add(jarBlob);
        List<OciDescriptor> descriptors = new ArrayList<>();
        OciDescriptor jarDescriptor = descriptor(jarBlob);
        jarDescriptor.setAnnotations(Map.of(MediaTypes.ANNOTATION_TITLE, config.getMainJar()));
        descriptors.add(jarDescriptor);
        for (ArtifactLayer layer : layers) {
            var content = blob(layer.tar(), layer.mediaType());
            blobs.add(content);
            OciDescriptor descriptor = descriptor(content);
            descriptor.setAnnotations(Map.of(MediaTypes.ANNOTATION_TITLE, layer.name()));
            descriptors.add(descriptor);
        }
        OciManifest manifest = new OciManifest();
        manifest.setArtifactType(MediaTypes.ARTIFACT_TYPE);
        manifest.setConfig(descriptor(configBlob));
        manifest.setLayers(descriptors);
        if (annotations != null && !annotations.isEmpty()) {
            manifest.setAnnotations(annotations);
        }
        return new ApplicationImage(blobs, List.of(),
                blob(json.writeValueAsBytes(manifest), MediaTypes.OCI_MANIFEST_MEDIA_TYPE));
    }

    private static RunnableImageBuilder.Blob blob(byte[] bytes, String mediaType) {
        return new RunnableImageBuilder.Blob(LocalStore.sha256Hex(bytes), bytes, mediaType);
    }

    private static OciDescriptor descriptor(RunnableImageBuilder.Blob blob) {
        return new OciDescriptor(blob.mediaType(), blob.digest(), blob.data().length);
    }
}
