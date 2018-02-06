import { Component, Input } from '@angular/core';

import { IntegrationOverview } from '@syndesis/ui/platform';

@Component({
  selector: 'syndesis-integration-status',
  template: `
    <div class="syndesis-integration-status">
      <!-- In Progress -->
      <div class="status pending" *ngIf="integration.currentState === 'Pending'">
        <div class="spinner spinner-sm spinner-inline"></div>
        In Progress
      </div>
      <!-- Status -->
      <div *ngIf="integration.currentState !== 'Pending'"
            class="status not-pending">
        <span class="label label-{{ getLabelClass(integration.currentState) }}">
          {{ getStatusText(integration.currentState) }}
        </span>
      </div>
    </div>
  `,
  styleUrls: ['./status.component.scss']
})
export class IntegrationStatusComponent {
  @Input() integration: IntegrationOverview;

  //-----  Get Status Icon Class ------------------->>
  getLabelClass(currentState): string | any {
    switch (currentState) {
      case 'Active':
        return 'primary';
      case 'Inactive':
        return 'inactive';
      case 'Undeployed':
        return 'danger';
      case 'Draft':
        return 'warning';
      default:
        return currentState;
    }
  }

  getStatusText(currentState): string | any {
    switch (currentState) {
      case 'Active':
        return 'Active';
      case 'Inactive':
        return 'Inactive';
      case 'Pending':
        return 'In Progress';
      default:
        return currentState;
    }
  }
}
