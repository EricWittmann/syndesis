/*
 * Copyright (C) 2016 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package io.syndesis.openshift;

import io.fabric8.kubernetes.api.model.EnvVar;
import io.fabric8.kubernetes.api.model.Quantity;
import io.fabric8.kubernetes.client.RequestConfigBuilder;
import io.fabric8.openshift.api.model.DeploymentConfig;
import io.fabric8.openshift.api.model.DeploymentConfigStatus;
import io.fabric8.openshift.api.model.User;
import io.fabric8.openshift.client.NamespacedOpenShiftClient;
import io.fabric8.openshift.client.OpenShiftClient;
import io.syndesis.core.Names;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.io.IOException;
import java.io.InputStream;
import java.util.List;
import java.util.Map;

@SuppressWarnings({"PMD.BooleanGetMethodName", "PMD.LocalHomeNamingConvention"})
public class OpenShiftServiceImpl implements OpenShiftService {
    private static final Logger LOGGER = LoggerFactory.getLogger(OpenShiftServiceImpl.class);

    private final NamespacedOpenShiftClient openShiftClient;
    private final OpenShiftConfigurationProperties config;

    public OpenShiftServiceImpl(NamespacedOpenShiftClient openShiftClient, OpenShiftConfigurationProperties config) {
        this.openShiftClient = openShiftClient;
        this.config = config;
    }

    @Override
    public void build(String name, DeploymentData deploymentData, InputStream tarInputStream) throws IOException {
        final String sName = Names.sanitize(name);

        ensureImageStreams(sName);
        ensureBuildConfig(sName, deploymentData, this.config.getBuilderImageStreamTag(), this.config.getImageStreamNamespace());
        openShiftClient.buildConfigs().withName(sName)
                       .instantiateBinary()
                       .fromInputStream(tarInputStream);
    }

    @Override
    public void deploy(String name, DeploymentData deploymentData) {
        final String sName = Names.sanitize(name);

        LOGGER.debug("Deploy {}", sName);

        ensureDeploymentConfig(sName, deploymentData);
        ensureSecret(sName, deploymentData);
    }

    @Override
    public boolean isDeploymentReady(String name) {
        String sName = Names.sanitize(name);
        return openShiftClient.deploymentConfigs().withName(sName).isReady();
    }

    @Override
    public boolean delete(String name) {
        final String sName = Names.sanitize(name);

        LOGGER.debug("Delete {}", sName);

        return
            removeImageStreams(sName) &&
            removeDeploymentConfig(sName) &&
            removeSecret(sName) &&
            removeBuildConfig(sName);
    }

    @Override
    public boolean exists(String name) {
        String sName = Names.sanitize(name);
        return openShiftClient.deploymentConfigs().withName(sName).get() != null;
    }

    @Override
    public void scale(String name, int desiredReplicas) {
        final String sName = Names.sanitize(name);

        LOGGER.debug("Scale {}", sName);

        openShiftClient.deploymentConfigs().withName(sName).edit()
                       .editSpec()
                       .withReplicas(desiredReplicas)
                       .endSpec()
                       .done();
    }


    @Override
    public boolean isScaled(String name, int desiredReplicas) {
        String sName = Names.sanitize(name);
        DeploymentConfig dc = openShiftClient.deploymentConfigs().withName(sName).get();

        int allReplicas = 0;
        int availableReplicas = 0;
        if (dc != null && dc.getStatus() != null) {
            DeploymentConfigStatus status = dc.getStatus();
            allReplicas = nullSafe(status.getReplicas());
            availableReplicas = nullSafe(status.getAvailableReplicas());
        }
        return desiredReplicas == allReplicas && desiredReplicas == availableReplicas;
    }

    @Override
    public boolean isBuildStarted(String name) {
        return checkBuildStatus(name, "Running");
    }

    @Override
    public boolean isBuildFailed(String name) {
        return checkBuildStatus(name, "Error");
    }

    protected boolean checkBuildStatus(String name, String status){
        String sName = Names.sanitize(name);
        return !openShiftClient.builds()
            .withLabel("openshift.io/build-config.name", sName)
            .withField("status", status)
            .list().getItems().isEmpty();
    }

    @Override
    public List<DeploymentConfig> getDeploymentsByLabel(Map<String, String> labels) {
        return openShiftClient.deploymentConfigs().withLabels(labels).list().getItems();
    }

    @Override
    public User whoAmI(String openShiftToken) {
        return openShiftClient.withRequestConfig(
            new RequestConfigBuilder().withOauthToken(openShiftToken).build()
        ).call(OpenShiftClient::currentUser);
    };

    private int nullSafe(Integer nr) {
        return nr != null ? nr : 0;
    }

    // ***********************
    // Image Stream
    // ***********************

    private void ensureImageStreams(String name) {
        LOGGER.debug("Create or Replace ImageStream {}", name);

        openShiftClient.imageStreams().withName(name).createOrReplaceWithNew()
            .withNewMetadata()
                .withName(name)
            .endMetadata()
            .done();
    }

    private boolean removeImageStreams(String name) {
        LOGGER.debug("Remove ImageStream {}", name);

        return openShiftClient.imageStreams().withName(name).delete();
    }

    protected void ensureDeploymentConfig(String name, DeploymentData deploymentData) {
        openShiftClient.deploymentConfigs().withName(name).createOrReplaceWithNew()
            .withNewMetadata()
                .withName(name)
                .addToAnnotations(deploymentData.getAnnotations())
                .addToLabels(deploymentData.getLabels())
            .endMetadata()
            .withNewSpec()
                .withReplicas(1)
                .addToSelector("integration", name)
                .withNewStrategy()
                    .withType("Recreate")
                    .withNewResources()
                       .addToLimits("memory", new Quantity(config.getDeploymentMemoryLimitMi()  + "Mi"))
                       .addToRequests("memory", new Quantity(config.getDeploymentMemoryRequestMi() +  "Mi"))
                    .endResources()
                .endStrategy()
                .withRevisionHistoryLimit(0)
                .withNewTemplate()
                    .withNewMetadata()
                        .addToLabels("integration", name)
                        .addToLabels(COMPONENT_LABEL, "integration")
                        .addToLabels(deploymentData.getLabels())
                        .addToAnnotations(deploymentData.getAnnotations())
                        .addToAnnotations("prometheus.io/scrape", "true")
                        .addToAnnotations("prometheus.io/port", "9779")
                    .endMetadata()
                    .withNewSpec()
                        .addNewContainer()
                        .withImage(" ")
                        .withImagePullPolicy("Always")
                        .withName(name)
                        // don't chain withEnv as every invocation overrides the previous one, use var-args instead
                        .withEnv(
                            new EnvVar("LOADER_HOME", config.getIntegrationDataPath(), null),
                            new EnvVar("AB_JMX_EXPORTER_CONFIG", "/tmp/src/prometheus-config.yml", null))
                        .addNewPort()
                            .withName("jolokia")
                            .withContainerPort(8778)
                        .endPort()
                        .addNewVolumeMount()
                            .withName("secret-volume")
                            .withMountPath("/deployments/config")
                            .withReadOnly(false)
                        .endVolumeMount()
                        .endContainer()
                        .addNewVolume()
                            .withName("secret-volume")
                            .withNewSecret()
                                .withSecretName(name)
                            .endSecret()
                        .endVolume()
                    .endSpec()
                .endTemplate()
                .addNewTrigger()
                    .withType("ImageChange")
                    .withNewImageChangeParams()
                        // set automatic to 'true' when not performing the deployments on our own
                        .withAutomatic(true)
                        .addToContainerNames(name)
                        .withNewFrom()
                            .withKind("ImageStreamTag")
                            .withName(name + ":latest")
                        .endFrom()
                    .endImageChangeParams()
                .endTrigger()
            .endSpec()
            .done();
    }



    private boolean removeDeploymentConfig(String projectName) {
        return openShiftClient.deploymentConfigs().withName(projectName).delete();
    }

    private void ensureBuildConfig(String name, DeploymentData deploymentData, String builderStreamTag, String imageStreamNamespace) {
        openShiftClient.buildConfigs().withName(name).createOrReplaceWithNew()
            .withNewMetadata()
                .withName(name)
                .addToAnnotations(deploymentData.getAnnotations())
                .addToLabels(deploymentData.getLabels())
            .endMetadata()
            .withNewSpec()
                .withRunPolicy("SerialLatestOnly")
                .withNewSource()
                    .withType("Binary")
                .endSource()
                .withNewStrategy()
                  .withType("Source")
                  .withNewSourceStrategy()
                    .withNewFrom()
                        .withKind("ImageStreamTag")
                        .withName(builderStreamTag)
                        .withNamespace(imageStreamNamespace)
                    .endFrom()
                    .withIncremental(false)
                    // TODO: This environment setup needs to be externalized into application.properties
                    // https://github.com/syndesisio/syndesis-rest/issues/682
                    .withEnv(
                        new EnvVar("MAVEN_OPTS", config.getMavenOptions(), null),
                        new EnvVar("BUILD_LOGLEVEL", config.isDebug() ? "5" : "1", null)
                    )
                  .endSourceStrategy()
                .endStrategy()
                .withNewOutput()
                    .withNewTo()
                    .withKind("ImageStreamTag")
                    .withName(name + ":latest")
                    .endTo()
                .endOutput()
            .endSpec()
         .done();
    }

    private boolean removeBuildConfig(String projectName) {
        return openShiftClient.buildConfigs().withName(projectName).delete();
    }

    private void ensureSecret(String name, DeploymentData deploymentData) {
        openShiftClient.secrets().withName(name).createOrReplaceWithNew()
            .withNewMetadata()
                .withName(name)
                .addToAnnotations(deploymentData.getAnnotations())
                .addToLabels(deploymentData.getLabels())
            .endMetadata()
            .withStringData(deploymentData.getSecret())
            .done();
    }


    private boolean removeSecret(String projectName) {
       return openShiftClient.secrets().withName(projectName).delete();
    }

}
